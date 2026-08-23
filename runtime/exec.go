package runtime

import "context"

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
type CountingExecutor struct {
	Inner Executor
	n     int
}

func (c *CountingExecutor) Query(ctx context.Context, sql string, args []any) (Rows, error) {
	c.n++
	return c.Inner.Query(ctx, sql, args)
}

func (c *CountingExecutor) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	c.n++
	return c.Inner.Exec(ctx, sql, args)
}

// RoundTrips reports how many statements have been issued.
func (c *CountingExecutor) RoundTrips() int { return c.n }

// Reset zeroes the counter.
func (c *CountingExecutor) Reset() { c.n = 0 }
