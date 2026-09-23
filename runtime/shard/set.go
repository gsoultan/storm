package shard

import (
	"context"
	"fmt"

	"github.com/gsoultan/storm/runtime"
)

// Set is the shard map: a locator, and the databases it places keys onto.
//
// The shards are held as a SLICE indexed by ID, not a map. A shard lookup
// happens on the path of every query against a sharded table, and the cost of
// that lookup should be a bounds check rather than a hash — which is also why
// ID is a position rather than a name. A Table locator is where names are
// allowed to live, and it pays for its map once, on the keys it was given.
type Set struct {
	loc    Locator
	shards []runtime.DB
}

// New builds a shard set. The shards are given in ID order: the first is
// shard 0.
//
// They are runtime.DB rather than runtime.Executor because a Set can Begin,
// and a thing that can begin a transaction is exactly what runtime.DB names.
// Taking Executor here and asserting for DB inside Begin would be the runtime
// capability sniff ADR-0005 rejected — and would turn "this deployment cannot
// do transactions" from a compile error into a 3am one.
func New(loc Locator, shards ...runtime.DB) (*Set, error) {
	if loc == nil {
		return nil, fmt.Errorf("storm/shard: New needs a locator")
	}
	if len(shards) == 0 {
		return nil, fmt.Errorf("storm/shard: New needs at least one shard")
	}
	if loc.N() != len(shards) {
		return nil, fmt.Errorf("storm/shard: the locator places keys over %d shards and New was given %d; "+
			"a key would resolve to a shard that is not here", loc.N(), len(shards))
	}
	for i, db := range shards {
		if db == nil {
			return nil, fmt.Errorf("storm/shard: shard %d is nil", i)
		}
	}
	return &Set{loc: loc, shards: append([]runtime.DB(nil), shards...)}, nil
}

// N is how many shards there are.
func (s *Set) N() int { return len(s.shards) }

// For resolves a key to the shard that holds its rows.
func (s *Set) For(k Key) (Bound, error) {
	id, err := s.loc.Locate(k)
	if err != nil {
		return nil, err
	}
	return s.ForID(id)
}

// ForID returns a shard by position, for the caller who already knows which
// one they want — a backfill walking shards in order, or an admin endpoint
// answering a question about shard 3.
func (s *Set) ForID(id ID) (Bound, error) {
	if id < 0 || int(id) >= len(s.shards) {
		return nil, fmt.Errorf("%w: shard %d, and there are %d (0 to %d)",
			ErrNoShard, id, len(s.shards), len(s.shards)-1)
	}
	return bound{ex: s.shards[id], id: id}, nil
}

// Begin starts a transaction on the one shard k resolves to.
//
// There is no Begin that spans shards. The caller who wants one wants
// atomicity across databases, which needs a durable coordinator and a
// recovery process — a deployed thing, and storm is imported rather than
// deployed. What storm does instead is refuse the write it can see crossing:
// see Unit.
func (s *Set) Begin(ctx context.Context, k Key) (BoundTx, error) {
	id, err := s.loc.Locate(k)
	if err != nil {
		return nil, err
	}
	if id < 0 || int(id) >= len(s.shards) {
		return nil, fmt.Errorf("%w: shard %d, and there are %d (0 to %d)",
			ErrNoShard, id, len(s.shards), len(s.shards)-1)
	}
	tx, err := s.shards[id].Begin(ctx)
	if err != nil {
		return nil, err
	}
	return boundTx{bound: bound{ex: tx, id: id}, tx: tx}, nil
}

// InTx runs fn inside one transaction on the one shard k resolves to, and
// commits it — or rolls back and returns why, including when fn panics. It is
// runtime.InTx with the routing done first.
func (s *Set) InTx(ctx context.Context, k Key, fn func(ex Bound) error) error {
	tx, err := s.Begin(ctx, k)
	if err != nil {
		return err
	}

	committed := false
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	committed = true
	return tx.Commit(ctx)
}

// Each calls fn once per shard, in ID order, and stops at the first error.
//
// This is the honest version of fan-out, and the name is deliberately not
// Query or All. storm will not run one query across every shard and merge the
// results, because a correct merge has to re-apply ORDER BY, LIMIT and keyset
// pagination across streams — and has no correct answer at all for AVG, for a
// window function, or for a COUNT DISTINCT. An ORM that returned one anyway
// would be returning a wrong number that looks exactly like a right one.
//
// So combining is the caller's, and the shape of the combination says whether
// it is sound. Summing a per-shard COUNT is sound. Averaging per-shard AVGs is
// not, and writing it out is where you notice.
//
// Serial rather than concurrent, on purpose: a backfill that opens every shard
// at once is a thundering herd, and a caller who wants concurrency has
// errgroup and the shard count from N.
func (s *Set) Each(ctx context.Context, fn func(ctx context.Context, id ID, ex Bound) error) error {
	for i, db := range s.shards {
		if err := ctx.Err(); err != nil {
			return err
		}
		id := ID(i)
		if err := fn(ctx, id, bound{ex: db, id: id}); err != nil {
			return fmt.Errorf("shard %d: %w", id, err)
		}
	}
	return nil
}
