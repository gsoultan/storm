package runtime

import (
	"context"
	"sync/atomic"
)

// Rows is the slice of a driver result that generated scanners need: raw wire
// bytes, nothing decoded.
type Rows interface {
	Next() bool
	RawValues() [][]byte
	Close()
	Err() error
}

// CopySource yields rows for a bulk load. It mirrors the shape every driver's
// COPY API takes without naming any driver's type — the same trick Rows plays
// for result sets.
type CopySource interface {
	Next() bool
	Values() []any
	Err() error
}

// BatchOp is one statement queued in a batch.
type BatchOp struct {
	SQL  string
	Args []any

	// WantRows says whether this statement's result is rows to scan or a count
	// of rows affected. A driver cannot supply both: the count arrives with the
	// result set's command tag, which is only readable once the rows are
	// closed, and closing them is what invalidates them.
	//
	// It is a field rather than a runtime probe because the generator already
	// knows — a statement has a RETURNING clause or it does not. Deciding it at
	// build time is the same rule the dialect follows.
	WantRows bool
}

// Executor is the driver port. Four methods, with a budget of five — see
// ADR-0005. Driver churn cannot reach the rest of the tree, and a test can
// count round trips by decorating it.
type Executor interface {
	Query(ctx context.Context, sql string, args []any) (Rows, error)
	Exec(ctx context.Context, sql string, args []any) (int64, error)

	// CopyFrom bulk-loads rows through the driver's copy protocol. This is a
	// different wire path, not a faster loop: it skips statement parsing and
	// per-row protocol overhead, which is why "1,000 inserts = one COPY" is a
	// gate storm can state and assert.
	//
	// An adapter whose driver has no copy protocol must emulate it and say so
	// in its package documentation. Silently degrading to 1,000 round trips
	// would make a performance claim depend on which adapter was passed.
	CopyFrom(ctx context.Context, table string, cols []string, src CopySource) (int64, error)

	// Batch sends several statements in one round trip and calls each with the
	// result of the i-th, in order. Returning an error from each aborts the
	// rest.
	//
	// A callback rather than a []Result because batch results are streamed:
	// each is valid only until the next is requested, so materialising them all
	// would mean copying every row or handing back handles already invalid.
	//
	// Exactly one of rows and affected is meaningful per op, chosen by
	// BatchOp.WantRows: rows is nil when the op wanted a count, and affected is
	// zero when it wanted rows.
	//
	// each may be nil, and a bulk insert or upsert usually passes nil: the
	// results are still drained in order — anything else desyncs the
	// connection — and the first error is returned. An implementation that
	// called a nil each would take the connection down with it.
	Batch(ctx context.Context, ops []BatchOp, each func(i int, rows Rows, affected int64, err error) error) error
}

// Transactions are deliberately absent. A transaction is an Executor you were
// given, not a method you call on one — which keeps Unit composable with
// whatever ownership model the caller already has, and keeps Begin, Commit and
// Rollback out of the five-method budget.

// CountingExecutor wraps an Executor and counts round trips. This is what
// proves the N+1 guarantee in tests — and it is exported so it can prove it in
// yours (see db.AssertRoundTrips in the docs).
//
// The counter is atomic because this is a *test* decorator and tests are where
// concurrency lives: a contention test that wrapped it would otherwise fail
// under -race, reporting a race in the tool rather than the bug it was hunting.
// An atomic increment costs nothing that matters next to a round trip.
type CountingExecutor struct {
	Inner Executor
	n     atomic.Int64
}

func (c *CountingExecutor) Query(ctx context.Context, sql string, args []any) (Rows, error) {
	c.n.Add(1)
	return c.Inner.Query(ctx, sql, args)
}

func (c *CountingExecutor) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	c.n.Add(1)
	return c.Inner.Exec(ctx, sql, args)
}

// CopyFrom counts as ONE round trip however many rows it loads. That is the
// whole claim: a bulk load is one conversation with the server, not one per
// row, and a test asserting it must see 1.
func (c *CountingExecutor) CopyFrom(ctx context.Context, table string, cols []string, src CopySource) (int64, error) {
	c.n.Add(1)
	return c.Inner.CopyFrom(ctx, table, cols, src)
}

// Batch counts as ONE round trip however many statements it carries, for the
// same reason.
func (c *CountingExecutor) Batch(ctx context.Context, ops []BatchOp, each func(int, Rows, int64, error) error) error {
	c.n.Add(1)
	return c.Inner.Batch(ctx, ops, each)
}

// RoundTrips reports how many statements have been issued.
func (c *CountingExecutor) RoundTrips() int { return int(c.n.Load()) }

// Reset zeroes the counter.
func (c *CountingExecutor) Reset() { c.n.Store(0) }
