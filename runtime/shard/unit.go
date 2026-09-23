package shard

import (
	"context"
	"errors"
	"fmt"

	"github.com/gsoultan/storm/runtime"
)

// ErrCrossShard means one unit of work staged writes for two different shards.
//
// It is returned at Add, not at Flush. The difference matters: at Add the
// caller is still inside the function that made the mistake and nothing has
// been sent, so the error names a bug in the code being written. At Flush the
// staging is over, the stack that chose the keys is gone, and the only honest
// options left are to refuse a unit that has already been assembled or to
// write half of it.
var ErrCrossShard = errors.New("storm/shard: one unit of work, two shards")

// Unit is a runtime.Unit pinned to one shard.
//
// A unit of work exists to make a graph of writes land or not land — its
// whole value is that a half-written graph is impossible. Across two
// databases storm cannot offer that, so it offers the next most useful
// thing: it notices, at the moment the second shard is staged, and says which
// two and which tenants put them there.
//
//	u := shard.NewUnit(ctx0.FlushOrder)
//	if err := u.Add(east, "orders", op1); err != nil { ... }
//	if err := u.Add(west, "orders", op2); err != nil { ... }
//	// storm/shard: one unit of work, two shards: "orders" is staged on
//	// shard 0 and "orders" on shard 3; a unit of work is one shard,
//	// because a transaction is. Split it into one unit per shard, or
//	// route both keys to the same shard.
//
// The advice in that message is the whole remedy and it is deliberately
// short, because there are only two remedies. Two units means two
// transactions and no atomicity between them, which is a design the caller
// has to accept on purpose. Co-locating the keys means choosing a shard key
// that puts the rows that change together in the same database — which is
// what sharding a transactional workload actually requires, and the thing an
// ORM cannot decide for you.
type Unit struct {
	inner *runtime.Unit

	// at is the shard every staged write belongs to, and first is the table
	// that put it there — kept so the refusal can name the write that set the
	// shard as well as the one that broke it.
	at    ID
	first string
	set   bool
}

// NewUnit builds a shard-pinned unit over a generated table ordering. The
// ordering is the same one runtime.NewUnit takes: it is computed at generate
// time from the foreign-key graph.
func NewUnit(rank map[string]int) *Unit {
	return &Unit{inner: runtime.NewUnit(rank)}
}

// Add stages a statement against a shard, and refuses a second one.
//
// The Bound is a parameter here where runtime.Unit.Add does not take an
// executor at all, and that is the point: the shard has to be known when the
// write is staged, because staging is the only moment at which both the
// offending write and the code that produced it are in view.
func (u *Unit) Add(ex Bound, table string, op runtime.BatchOp) error {
	id := ex.Shard()
	if !u.set {
		u.at, u.first, u.set = id, table, true
	} else if id != u.at {
		return fmt.Errorf("%w: %q is staged on shard %d and %q on shard %d; "+
			"a unit of work is one shard, because a transaction is. Split it into one unit "+
			"per shard, or route both keys to the same shard",
			ErrCrossShard, u.first, u.at, table, id)
	}
	u.inner.Add(table, op)
	return nil
}

// Len reports how many statements are staged.
func (u *Unit) Len() int { return u.inner.Len() }

// Shard is the shard every staged write belongs to, and false if nothing has
// been staged yet.
func (u *Unit) Shard() (ID, bool) { return u.at, u.set }

// Flush sends every staged statement in foreign-key order, as one round trip,
// to the shard they were all staged against.
//
// The Bound is taken here as well as at Add, and checked against it, because
// the two are different executors in the case that matters: writes are staged
// against Set.For's pool-backed Bound and flushed against Set.InTx's
// transaction-backed one. Both name a shard, so storm can confirm they name
// the SAME shard — and a flush aimed at the wrong database is caught before
// the round trip rather than after it.
func (u *Unit) Flush(ctx context.Context, ex Bound) ([]int64, error) {
	if u.set && ex.Shard() != u.at {
		return nil, fmt.Errorf("%w: staged on shard %d and flushed against shard %d",
			ErrCrossShard, u.at, ex.Shard())
	}
	return u.inner.Flush(ctx, ex)
}
