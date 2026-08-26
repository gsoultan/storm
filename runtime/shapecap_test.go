package runtime_test

import (
	"fmt"
	goruntime "runtime"
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
