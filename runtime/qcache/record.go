package qcache

import (
	"context"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// recorder passes a result through to its reader and keeps a copy, which is
// stored when the reader has seen every row and closes.
//
// Only then: a reader that stops early (a First, a Take, an error in the
// scanner) has seen a prefix, and a prefix stored as the whole answer would be
// a wrong answer on the next hit. A result that outgrows Options.MaxBytes
// drops its copy as soon as it does, so a large read costs no more memory for
// being marked cacheable.
type recorder struct {
	inner runtime.Rows
	vals  runtime.ValueRows // non-nil when inner is value-shaped
	c     *Cache
	ctx   context.Context
	key   string
	ttl   time.Duration
	buf   []byte
	done  bool // inner.Next returned false with no error
	drop  bool // the copy was abandoned
	shut  bool
}

func (r *recorder) Next() bool {
	if !r.inner.Next() {
		r.done = r.inner.Err() == nil
		return false
	}
	if r.drop {
		return true
	}
	if r.vals != nil {
		var ok bool
		if r.buf, ok = appendValueRow(r.buf, r.vals.Values()); !ok {
			r.abandon()
			return true
		}
	} else {
		r.buf = appendByteRow(r.buf, r.inner.RawValues())
	}
	if len(r.buf) > r.c.opt.MaxBytes {
		r.abandon()
	}
	return true
}

func (r *recorder) abandon() { r.drop, r.buf = true, nil }

func (r *recorder) RawValues() [][]byte { return r.inner.RawValues() }
func (r *recorder) Err() error          { return r.inner.Err() }

func (r *recorder) Close() {
	if r.shut {
		return
	}
	r.shut = true
	r.inner.Close()
	if !r.done || r.drop || r.inner.Err() != nil {
		return
	}
	r.c.put(r.ctx, r.key, r.buf, r.ttl)
	r.buf = nil
}

// valueRecorder is recorder with Values: the value shape, runtime.ValueRows.
type valueRecorder recorder

func (v *valueRecorder) Next() bool          { return (*recorder)(v).Next() }
func (v *valueRecorder) RawValues() [][]byte { return v.inner.RawValues() }
func (v *valueRecorder) Err() error          { return v.inner.Err() }
func (v *valueRecorder) Close()              { (*recorder)(v).Close() }
func (v *valueRecorder) Values() []any       { return v.vals.Values() }
