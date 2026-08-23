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

// Executor is the driver port. It is five methods and stays five methods, so
// driver churn cannot reach the rest of the tree and tests can count round
// trips by decorating it.
type Executor interface {
	Query(ctx context.Context, sql string, args []any) (Rows, error)
	Exec(ctx context.Context, sql string, args []any) (int64, error)
}

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

// RoundTrips reports how many statements have been issued.
func (c *CountingExecutor) RoundTrips() int { return int(c.n.Load()) }

// Reset zeroes the counter.
func (c *CountingExecutor) Reset() { c.n.Store(0) }
