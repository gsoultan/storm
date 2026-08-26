package runtime_test

import (
	"errors"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"

	"github.com/gsoultan/raorm/runtime"
)

// mintShapes fills a cache with n distinct token streams, each carrying a
// statement about the size a real one is, so what the cache retains is
// measurable rather than notional.
func mintShapes(c *runtime.TreeCache, n int) {
	for i := 0; i < n; i++ {
		toks := []runtime.Tok{
			runtime.MakeLeaf(uint32(1+i%40), uint32(i%900)),
			runtime.MakeLeaf(uint32(1+(i/40)%40), uint32((i/900)%900)),
			runtime.MakeGroup(runtime.KAnd, 2),
		}
		if c.Get(toks) == nil {
			c.Put(toks, &runtime.Stmt{
				SQL: fmt.Sprintf(
					`SELECT "id", "email", "org_id", "created_at" FROM "users" WHERE "col_%d" = $1 AND "col_%d" = $2 ORDER BY "id" LIMIT $3`,
					i%900, (i/900)%900),
				NArg: 3,
			})
		}
	}
}

// heapAfterGC is the retained-bytes reading: two collections, because the
// first can leave the second's sweep incomplete and read high.
func heapAfterGC() uint64 {
	goruntime.GC()
	goruntime.GC()
	var m goruntime.MemStats
	goruntime.ReadMemStats(&m)
	return m.HeapAlloc
}

// A builder fed request data can mint shapes without limit — n optional
// filters is up to 2ⁿ — and before the cap that meant a map that only ever
// grew, retaining one compiled statement per shape for the life of the
// process. This asserts the ceiling holds, and (the half that makes it a
// tripwire rather than a claim) that the same workload is genuinely unbounded
// when the cap is off, so a regression that silently stopped enforcing it
// cannot pass.
func TestShapeCap_BoundsAShapeExplosion(t *testing.T) {
	const minted = 100_000

	prev := runtime.ShapeCap()
	t.Cleanup(func() { runtime.SetShapeCap(prev) })

	// Unbounded: the old behaviour, kept reachable and asserted, so the
	// bounded case below is measured against something real.
	runtime.SetShapeCap(0)
	loose := runtime.NewTreeCache()
	base := heapAfterGC()
	mintShapes(loose, minted)
	looseHeap := heapAfterGC() - base
	looseShapes := loose.Shapes()
	if looseShapes != minted {
		t.Fatalf("unbounded cache holds %d of %d shapes — the workload is not minting what the test thinks",
			looseShapes, minted)
	}
	if loose.Flushes() != 0 {
		t.Fatalf("unbounded cache flushed %d times", loose.Flushes())
	}
	loose = nil

	// Bounded: same workload, same statements, ceiling enforced.
	const cap = 1024
	runtime.SetShapeCap(cap)
	tight := runtime.NewTreeCache()
	base = heapAfterGC()
	mintShapes(tight, minted)
	tightHeap := heapAfterGC() - base
	tightShapes := tight.Shapes()

	t.Logf("100k shapes: unbounded holds %d (%d KB retained), capped holds %d (%d KB retained), %d flushes",
		looseShapes, looseHeap/1024, tightShapes, tightHeap/1024, tight.Flushes())

	if tightShapes > cap {
		t.Fatalf("cache holds %d shapes, cap is %d", tightShapes, cap)
	}
	if tight.Flushes() == 0 {
		t.Fatal("100k shapes past a cap of 1024 must have flushed")
	}
	// The ceiling has to show up as memory, not just as a counter.
	if tightHeap > looseHeap/8 {
		t.Fatalf("capped cache retained %d KB against the unbounded %d KB — the bound is not bounding anything",
			tightHeap/1024, looseHeap/1024)
	}
	goruntime.KeepAlive(tight)
}

// Flushing must not break the cache: what is still in use recompiles once and
// is resident again, and statements handed out before a flush stay valid,
// because a caller holds the pointer rather than the map.
func TestShapeCap_SurvivesFlushing(t *testing.T) {
	prev := runtime.ShapeCap()
	t.Cleanup(func() { runtime.SetShapeCap(prev) })
	runtime.SetShapeCap(16)

	c := runtime.NewTreeCache()
	hot := []runtime.Tok{runtime.MakeLeaf(1, 1)}
	held := c.Put(hot, &runtime.Stmt{SQL: "SELECT 1", NArg: 0})

	mintShapes(c, 500)
	if c.Flushes() == 0 {
		t.Fatal("expected flushes")
	}
	if held.SQL != "SELECT 1" {
		t.Fatal("a statement handed out before a flush was mutated")
	}
	// Still usable: a miss recompiles, and the next lookup hits again.
	if got := c.Get(hot); got != nil && got.SQL != "SELECT 1" {
		t.Fatalf("stale entry after flush: %q", got.SQL)
	}
	again := c.Put(hot, &runtime.Stmt{SQL: "SELECT 1", NArg: 0})
	if c.Get(hot) != again {
		t.Fatal("cache does not serve what it just stored")
	}
	if c.Shapes() > 16 {
		t.Fatalf("holds %d shapes past a cap of 16", c.Shapes())
	}
}

// The default is a real number, not zero: a program that never touches
// SetShapeCap is bounded.
func TestShapeCap_DefaultIsBounded(t *testing.T) {
	if runtime.ShapeCap() <= 0 {
		t.Fatalf("default cap is %d — unbounded by default is the bug this closed", runtime.ShapeCap())
	}
}

// An array of a user-defined type (enum[]) arrives in TEXT format, because
// pgx has no binary codec for one. The executor's format guard lets
// user-defined OIDs through deliberately — a scalar enum's label IS its value
// — so the array case lands here, at the decoder, and must say what actually
// went wrong. Before this it reported "multi-dimensional array ... it has
// 2069982320", which is four bytes of "{alp" read as a dimension count: true,
// useless, and pointing at the wrong feature.
//
// Bytes are the real ones, captured from `ARRAY['alpha','beta']::my_enum[]`.
func TestArray_TextFormatIsNamed(t *testing.T) {
	var sl runtime.Slab
	_, err := runtime.TextArray([]byte("{alpha,beta}"), &sl)
	if !errors.Is(err, runtime.ErrArrayTextFormat) {
		t.Fatalf("text-format array reported %v, want ErrArrayTextFormat", err)
	}
	for _, want := range []string{"TEXT format", "enum[]", "text[]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}

	// The binary path is untouched: a real text[] still decodes.
	binary := []byte{
		0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 25, 0, 0, 0, 2, 0, 0, 0, 1,
		0, 0, 0, 1, 'x', 0, 0, 0, 1, 'y',
	}
	got, err := runtime.TextArray(binary, &sl)
	if err != nil {
		t.Fatalf("binary text[] must still decode: %v", err)
	}
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("binary text[] decoded as %#v", got)
	}
}

// Flushing replaces the map while other goroutines are reading it, so the
// design has to be right rather than lucky: Put holds the write lock, and Get
// copies the bucket slice header under a read lock before iterating it, so a
// reader can hold entries the map no longer references and still walk them
// safely. Append never mutates what a reader can see — it writes past the
// reader's len or allocates a new array.
//
// This hammers that path with the cap set low enough that flushes happen
// continuously, and asserts every reader gets a usable statement rather than
// a torn or empty one. It exists to be run under -race.
func TestShapeCap_ConcurrentGetPutWhileFlushing(t *testing.T) {
	prev := runtime.ShapeCap()
	t.Cleanup(func() { runtime.SetShapeCap(prev) })
	runtime.SetShapeCap(32)

	c := runtime.NewTreeCache()
	hot := []runtime.Tok{runtime.MakeLeaf(1, 7)}
	const hotSQL = "SELECT hot"
	c.Put(hot, &runtime.Stmt{SQL: hotSQL, NArg: 1})

	var wg sync.WaitGroup
	const writers, readers, each = 4, 4, 2000

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				toks := []runtime.Tok{
					runtime.MakeLeaf(uint32(1+(i+w)%30), uint32((i*7+w)%900)),
					runtime.MakeLeaf(uint32(1+(i/30+w)%30), uint32((i*13+w)%900)),
					runtime.MakeGroup(runtime.KAnd, 2),
				}
				if c.Get(toks) == nil {
					c.Put(toks, &runtime.Stmt{SQL: "SELECT churn", NArg: 2})
				}
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				// The hot shape is either present or was just flushed; what
				// must never happen is a statement that is neither.
				if st := c.Get(hot); st != nil && st.SQL != hotSQL {
					t.Errorf("read a torn statement: %q", st.SQL)
					return
				}
			}
		}()
	}
	wg.Wait()

	if c.Shapes() > 32 {
		t.Fatalf("holds %d shapes past a cap of 32 after concurrent use", c.Shapes())
	}
	if c.Flushes() == 0 {
		t.Fatal("the workload never flushed — it is not exercising the path it claims to")
	}
	t.Logf("concurrent churn: %d shapes held, %d flushes", c.Shapes(), c.Flushes())
}
