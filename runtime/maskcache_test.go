package runtime

import (
	"sync"
	"testing"
)

// A MaskCache hit must be the statement for the mask that was asked for.
//
// The hot path stored the mask and the statement in two separate atomics, so
// two goroutines interning different masks could interleave their stores and
// leave the pair mismatched: last from one, hot from the other. A reader then
// got a compiled UPDATE for a DIFFERENT column set and bound its arguments
// against those placeholders — the wrong columns written, or a type error,
// depending on what the two masks happened to be.
//
// This is not a data race; every access is atomic. -race cannot see it.
func TestMaskCacheHitMatchesTheMaskAskedFor(t *testing.T) {
	const (
		rounds  = 300
		perMask = 8
		spins   = 4000
	)

	for r := 0; r < rounds; r++ {
		c := NewMaskCache()
		a := &Stmt{SQL: "A", NArg: 1}
		b := &Stmt{SQL: "B", NArg: 2}
		c.Put(1, a)
		c.Put(2, b)

		var wg sync.WaitGroup
		bad := make(chan string, perMask*2)
		start := make(chan struct{})
		for _, tc := range []struct {
			mask uint64
			want *Stmt
		}{{1, a}, {2, b}} {
			for g := 0; g < perMask; g++ {
				wg.Add(1)
				go func(mask uint64, want *Stmt) {
					defer wg.Done()
					<-start
					for i := 0; i < spins; i++ {
						if got := c.Get(mask); got != nil && got != want {
							select {
							case bad <- "statement " + got.SQL + " returned for mask of " + want.SQL:
							default:
							}
							return
						}
					}
				}(tc.mask, tc.want)
			}
		}
		close(start)
		wg.Wait()
		close(bad)
		if msg, ok := <-bad; ok {
			t.Fatalf("round %d: %s", r, msg)
		}
	}
}

// The warm path is on every write. It must not allocate.
func TestMaskCacheGetDoesNotAllocate(t *testing.T) {
	c := NewMaskCache()
	c.Put(1, &Stmt{SQL: "A"})
	c.Put(2, &Stmt{SQL: "B"})
	if n := testing.AllocsPerRun(1000, func() {
		c.Get(1)
		c.Get(2)
	}); n != 0 {
		t.Fatalf("Get allocates %v times per pair; the entry is interned and should just be published", n)
	}
}
