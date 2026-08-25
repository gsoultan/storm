package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Unit is a set of writes flushed together, in an order that satisfies foreign
// keys, as one round trip.
//
// # Ordering, and why it is not the database's job
//
// Postgres will forgive a wrong order if every constraint is DEFERRABLE
// INITIALLY DEFERRED, and plenty of ORMs quietly rely on that. raorm does not:
// deferring a constraint moves the failure from the statement that caused it to
// COMMIT, where the error names a constraint and not the write that violated
// it, and it only works on constraints somebody remembered to declare
// deferrable. Ordering the writes is the ORM's job, and the test for it runs
// with constraints NOT deferred so the database cannot cover a mistake.
//
// # No deferred id handles
//
// docs/API.md §8 sketched handles — insert a parent, get a placeholder, use it
// as a child's foreign key before the parent is written. raorm does not need
// them: raorm.Model's id is a client-generated UUID, so the parent's key is
// known before the insert rather than after it. Handles are only unavoidable
// when the database assigns the key, which is the sequence-id model raorm does
// not use. If a table ever does, this is where that machinery goes.
type Unit struct {
	// rank orders tables so that a table's dependencies flush before it. It is
	// computed at GENERATE time from the foreign-key graph and passed in, so no
	// runtime code inspects a schema.
	rank map[string]int

	ops []unitOp
}

type unitOp struct {
	table string
	seq   int // declaration order, to keep the sort stable within a table
	op    BatchOp
}

// NewUnit builds a unit over a generated table ordering.
func NewUnit(rank map[string]int) *Unit { return &Unit{rank: rank} }

// Add stages a statement. Nothing is sent until Flush.
func (u *Unit) Add(table string, op BatchOp) {
	u.ops = append(u.ops, unitOp{table: table, seq: len(u.ops), op: op})
}

// Len reports how many statements are staged.
func (u *Unit) Len() int { return len(u.ops) }

// ErrUnknownTable means a statement was staged for a table the generated
// ordering does not cover — usually a write from another bounded context, which
// cannot be ordered against this one because the foreign-key graph does not
// span them.
var ErrUnknownTable = errors.New("raorm: no flush order for table")

// Flush sends every staged statement in foreign-key order as ONE round trip,
// and reports the rows each affected.
//
// Writes to the same table keep the order they were added: within a table the
// caller's sequence is the only information available, and reordering it would
// break a delete-then-insert of the same key.
func (u *Unit) Flush(ctx context.Context, ex Executor) ([]int64, error) {
	if len(u.ops) == 0 {
		return nil, nil
	}
	for _, o := range u.ops {
		if _, ok := u.rank[o.table]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrUnknownTable, o.table)
		}
	}

	ordered := make([]unitOp, len(u.ops))
	copy(ordered, u.ops)
	sort.SliceStable(ordered, func(i, j int) bool {
		ri, rj := u.rank[ordered[i].table], u.rank[ordered[j].table]
		if ri != rj {
			return ri < rj
		}
		return ordered[i].seq < ordered[j].seq
	})

	ops := make([]BatchOp, len(ordered))
	for i, o := range ordered {
		ops[i] = o.op
	}

	affected := make([]int64, len(ops))
	var first error
	err := ex.Batch(ctx, ops, func(i int, _ Rows, n int64, err error) error {
		if err != nil && first == nil {
			first = fmt.Errorf("raorm: unit statement %d (%s): %w", i, ordered[i].table, err)
		}
		affected[i] = n
		return nil
	})
	if err != nil {
		return affected, err
	}
	if first != nil {
		return affected, first
	}
	// Clear before truncating: the backing array survives for the next use,
	// and each op's Args reference caller values — a long-lived Unit would
	// otherwise pin every argument of every flush it ever made.
	for i := range u.ops {
		u.ops[i] = unitOp{}
	}
	u.ops = u.ops[:0]
	return affected, nil
}
