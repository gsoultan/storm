package runtime

import "unsafe"

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
