package hooks

import (
	"context"

	"github.com/gsoultan/storm/runtime"
)

// rows counts what the caller reads and runs After when the caller closes
// the result, so a Query's Duration covers the rows and not just the first
// byte.
type rows struct {
	inner runtime.Rows
	e     *executor
	ctx   context.Context
	s     *Statement
	n     int64
	done  bool
}

func (r *rows) Next() bool {
	if r.inner.Next() {
		r.n++
		return true
	}
	return false
}

func (r *rows) RawValues() [][]byte { return r.inner.RawValues() }
func (r *rows) Err() error          { return r.inner.Err() }

// Close is safe to call twice, and After runs on the first.
func (r *rows) Close() {
	r.inner.Close()
	if r.done {
		return
	}
	r.done = true
	r.e.end(r.ctx, r.s, r.n, r.inner.Err())
}
