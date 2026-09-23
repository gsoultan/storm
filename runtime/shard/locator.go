package shard

import "fmt"

// Locator decides which shard a key belongs to.
//
// One method, and it takes the Key rather than a hash of it, because the two
// useful families of locator need different things: Jump and Modulo want the
// hash, and Table wants the key itself. Handing a hash to Table would make
// moving one tenant to another shard impossible without rehashing every other
// tenant — which is the operation lookup sharding exists to make cheap.
type Locator interface {
	// Locate returns the shard for a key, or ErrNoShard.
	Locate(k Key) (ID, error)

	// N is how many shards this locator was built for. Set checks it against
	// how many it was given, so a locator built for four shards and a Set
	// holding three is an error at construction rather than a key that
	// resolves to a shard that is not there.
	N() int
}

// Jump places a key with jump consistent hash over n shards.
//
// The reason to prefer it over Modulo is what happens when n changes: growing
// from 4 shards to 5 moves 1/5 of the keys, and Modulo moves about 4/5 of
// them. That difference is the whole cost of adding a shard, so it is worth a
// loop of a dozen iterations per query.
//
// It is still a rehash. Adding a shard moves keys, which means moving rows,
// which means a migration with a cutover — see the package note on
// resharding. Jump makes that migration small; it does not make it unneeded.
type Jump int

var _ Locator = Jump(0)

func (j Jump) N() int { return int(j) }

func (j Jump) Locate(k Key) (ID, error) {
	if j <= 0 {
		return 0, fmt.Errorf("%w: a Jump locator over %d shards", ErrNoShard, int(j))
	}
	if k.kind == kindInvalid {
		return 0, fmt.Errorf("%w: the key is the zero Key, so no column was read into it", ErrNoShard)
	}
	// Lamping & Veach's jump consistent hash. b is the last bucket the key
	// jumped to; the loop jumps forward until it passes n.
	key := k.hash()
	var b, i int64 = -1, 0
	for i < int64(j) {
		b = i
		key = key*2862933555777941757 + 1
		i = int64(float64(b+1) * (float64(int64(1)<<31) / float64((key>>33)+1)))
	}
	return ID(b), nil
}

// Modulo places a key with hash % n.
//
// Here because it is what an adopter with a fixed shard count and a migration
// plan already written actually wants, and because being explicit about it is
// better than having them discover Jump moves keys differently than the shell
// script they have been using. Prefer Jump if the shard count will ever
// change.
type Modulo int

var _ Locator = Modulo(0)

func (m Modulo) N() int { return int(m) }

func (m Modulo) Locate(k Key) (ID, error) {
	if m <= 0 {
		return 0, fmt.Errorf("%w: a Modulo locator over %d shards", ErrNoShard, int(m))
	}
	if k.kind == kindInvalid {
		return 0, fmt.Errorf("%w: the key is the zero Key, so no column was read into it", ErrNoShard)
	}
	return ID(k.hash() % uint64(m)), nil
}

// Table places a key by looking it up, and is the right locator for
// multi-tenant sharding.
//
// A hash decides where a tenant lives and takes the decision away from you. A
// table lets you put the one tenant that outgrew its neighbours on a shard of
// its own, move a tenant without touching any other, and answer "where does
// this customer's data live" from a map you can read. The cost is that the
// map has to be built — from config, or from a directory table read at start
// up — and that an unknown key is ErrNoShard rather than an arbitrary shard.
//
// That last part is a feature. A hash locator answers a typo with a real
// shard and a silently empty result; this one says the key is not a tenant.
type Table struct {
	to map[Key]ID
	n  int
}

var _ Locator = (*Table)(nil)

// NewTable builds a lookup locator over n shards. Every ID in the map must be
// in range, which is checked here rather than per query.
func NewTable(n int, to map[Key]ID) (*Table, error) {
	if n <= 0 {
		return nil, fmt.Errorf("storm/shard: a Table locator over %d shards", n)
	}
	m := make(map[Key]ID, len(to))
	for k, id := range to {
		if id < 0 || int(id) >= n {
			return nil, fmt.Errorf("storm/shard: key %s maps to shard %d, and there are %d shards (0 to %d)",
				k, id, n, n-1)
		}
		if k.kind == kindInvalid {
			return nil, fmt.Errorf("storm/shard: the zero Key is in the lookup table; it matches no column value")
		}
		m[k] = id
	}
	return &Table{to: m, n: n}, nil
}

func (t *Table) N() int { return t.n }

func (t *Table) Locate(k Key) (ID, error) {
	id, ok := t.to[k]
	if !ok {
		return 0, fmt.Errorf("%w: %s is not in the lookup table", ErrNoShard, k)
	}
	return id, nil
}
