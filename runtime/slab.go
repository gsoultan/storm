package runtime

import (
	"errors"
	"iter"
	"unsafe"
)

// Slab is a chunked string arena. Decoding a result copies every text column
// into it once, so a 1,000-row scan costs a handful of allocations instead of
// three per row.
//
// Chunks are never reallocated — a full chunk is retired to `full` and a new
// one started — so a string handed out earlier can never be invalidated by a
// later append. That property is what makes unsafe.String safe here.
//
// Lifetime: strings returned by a Slab live as long as the Slab. A result set
// therefore shares backing memory, exactly like pgx's RawValues. Slabs are NOT
// pooled by default: reusing one would corrupt strings still held by the
// previous result, and that is not a trade an ORM should make silently.
type Slab struct {
	cur  []byte
	full [][]byte
}

const (
	slabMin = 1 << 7  // 128 B — a single row must not pay for a bulk chunk
	slabMax = 1 << 16 // 64 KiB
)

// EachSlabCap is how much text a streaming read lets one Slab hold before it
// starts another. One full chunk: past it the old arena is left to the
// collector — once no string a caller kept points into it — so a read of any
// length holds about this much text, not the whole result.
const EachSlabCap = slabMax

// Seq turns a generated Each into an iterator, which is what a generated
// Rows returns:
//
//	for r, err := range user.New().Where(...).Rows(ctx, ex) {
//		if err != nil {
//			return err
//		}
//		...
//	}
//
// It reads in the same constant memory as Each, because it is Each. Leaving
// the loop early stops the read and closes the result; an error from the
// query or the scan is yielded once, with a zero row, and ends the loop.
func Seq[R any](each func(fn func(R) error) error) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		err := each(func(r R) error {
			if !yield(r, nil) {
				return errStopSeq
			}
			return nil
		})
		if err != nil && err != errStopSeq {
			var zero R
			yield(zero, err)
		}
	}
}

// errStopSeq is how Seq tells Each the loop body broke out. It never leaves
// Seq, so nobody can compare against it.
var errStopSeq = errors.New("storm: iteration stopped")

// Bytes copies b into the arena and returns the copy.
//
// Same contract as Str: the result lives as long as the Slab and must not be
// retained past the result set. Used for jsonb, where the caller unmarshals
// immediately and keeps the decoded value rather than the bytes.
//
// The slice is capped to its length so appending to it cannot reach into the
// next value's storage — a shared arena makes that a real hazard, not a
// theoretical one.
func (s *Slab) Bytes(b []byte) []byte {
	n := len(b)
	if n == 0 {
		return nil
	}
	if cap(s.cur)-len(s.cur) < n {
		s.grow(n)
	}
	off := len(s.cur)
	s.cur = append(s.cur, b...)
	return s.cur[off : off+n : off+n]
}

// str copies b into the arena and returns it as a string.
func (s *Slab) Str(b []byte) string {
	n := len(b)
	if n == 0 {
		return ""
	}
	if cap(s.cur)-len(s.cur) < n {
		s.grow(n)
	}
	off := len(s.cur)
	s.cur = append(s.cur, b...)
	// Safe: s.cur has spare capacity, so append did not move the backing array,
	// and the chunk is retired rather than reallocated once it fills.
	return unsafe.String(&s.cur[off], n)
}

func (s *Slab) grow(n int) {
	if len(s.cur) > 0 {
		s.full = append(s.full, s.cur)
	}
	size := slabMin
	if c := cap(s.cur) * 2; c > size {
		size = c
	}
	if size > slabMax {
		size = slabMax
	}
	if n > size {
		size = n
	}
	s.cur = make([]byte, 0, size)
}

// Reserve pre-sizes the arena to n bytes. Only meaningful before first use:
// chunk doubling from a cold start over-allocates roughly 1.8x, which costs
// more memory traffic than the saved mallocs are worth at high throughput.
func (s *Slab) Reserve(n int) {
	if n <= 0 || len(s.cur) > 0 || cap(s.cur) >= n {
		return
	}
	if n > slabMax {
		n = slabMax
	}
	s.cur = make([]byte, 0, n)
}

// Size reports how many bytes the arena is holding, for tests and lint.
func (s *Slab) Size() int {
	n := len(s.cur)
	for _, c := range s.full {
		n += len(c)
	}
	return n
}
