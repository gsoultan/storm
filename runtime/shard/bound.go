package shard

import (
	"context"

	"github.com/gsoultan/storm/runtime"
)

// Bound is an Executor that has been resolved to one shard.
//
// This type is the compile-time half of storm's sharding. Generated code for
// a sharded model takes a Bound where an unsharded model takes a
// runtime.Executor, so the mistake that matters —
//
//	us, err := user.New().All(ctx, pool, nil)   // which tenant's users?
//
// — does not compile. It is the same move as every other storm guarantee: the
// check that cannot be made later is made by the type system now, rather than
// by a runtime assertion nobody reads until production.
//
// Shard is exported so a log line, a metric or a test can say which shard ran
// the query. The unexported method is what keeps a plain pool from satisfying
// the interface by accident; Pin is the sanctioned way to make one by hand.
type Bound interface {
	runtime.Executor

	// Shard is which shard this executor was resolved to.
	Shard() ID

	// boundToOneShard has no body and no callers. It exists so that
	// satisfying Bound is a deliberate act performed in this package.
	boundToOneShard()
}

// BoundTx is a transaction on one shard.
//
// There is no cross-shard BoundTx and there will not be one: see the package
// note, and Unit for what storm does when it can see a write crossing.
type BoundTx interface {
	Bound

	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// bound is the ordinary Bound: an Executor plus the shard it belongs to.
type bound struct {
	ex runtime.Executor
	id ID
}

var _ Bound = bound{}

func (b bound) Shard() ID      { return b.id }
func (bound) boundToOneShard() {}

func (b bound) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	return b.ex.Query(ctx, sql, args)
}

func (b bound) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	return b.ex.Exec(ctx, sql, args)
}

func (b bound) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	return b.ex.CopyFrom(ctx, table, cols, src)
}

func (b bound) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {
	return b.ex.Batch(ctx, ops, each)
}

// boundTx is a Bound whose Executor is a transaction.
type boundTx struct {
	bound
	tx runtime.Tx
}

var _ BoundTx = boundTx{}

func (b boundTx) Commit(ctx context.Context) error   { return b.tx.Commit(ctx) }
func (b boundTx) Rollback(ctx context.Context) error { return b.tx.Rollback(ctx) }

// Pin declares that ex holds shard id's rows, without going through a Set.
//
// Two uses, both real:
//
//   - A TEST. runtime.CountingExecutor is exported so an adopter can prove the
//     N+1 guarantee in their own suite, and a sharded model's methods take a
//     Bound, so there has to be a door from one to the other:
//
//     ce := &runtime.CountingExecutor{Inner: ex}
//     us, err := user.New().All(ctx, shard.Pin(0, ce), nil)
//
//   - ONE shard. A model declared as sharded that is, today, deployed on a
//     single database. Pin(0, pool) says so at the seam, in one place, rather
//     than by leaving the shard key off the model and having to add it back
//     across the whole codebase on the day a second shard appears.
//
// Pin does not check anything. It is an assertion by the caller, which is why
// it is named for one.
func Pin(id ID, ex runtime.Executor) Bound { return bound{ex: ex, id: id} }
