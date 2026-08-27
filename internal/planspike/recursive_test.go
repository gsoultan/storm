package planspike_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gsoultan/storm/internal/planspike/store/org"
	"github.com/gsoultan/storm/runtime"
)

// tree builds a chain root -> a -> b -> c and returns the ids top-down.
func tree(t *testing.T, ctx context.Context, ex runtime.Executor, prefix string, depth int) [][16]byte {
	t.Helper()
	ids := make([][16]byte, depth)
	var parent [16]byte
	for i := range depth {
		n := org.Create()
		id := newID()
		n.SetID(id)
		n.SetName(prefix + "-" + string(rune('a'+i)))
		if i > 0 {
			n.SetParentID(parent)
		}
		r, err := n.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = r.ID
		parent = r.ID
	}
	t.Cleanup(func() {
		// Children first: the FK is ON DELETE CASCADE, but deleting bottom-up
		// keeps the cleanup honest about ordering.
		for i := len(ids) - 1; i >= 0; i-- {
			_ = org.Delete(ctx, ex, ids[i])
		}
	})
	return ids
}

// A whole subtree in one query.
func TestDescend_ReturnsTheSubtreeInOneRoundTrip(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	ids := tree(t, ctx, ex, "desc", 4)

	count.Reset()
	rows, err := org.Descend(ctx, ex, ids[:1], 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want the root and three descendants", len(rows))
	}
	if n := count.RoundTrips(); n != 1 {
		t.Errorf("%d round trips for a four-level tree, want 1", n)
	}
	got := map[[16]byte]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	for i, id := range ids {
		if !got[id] {
			t.Errorf("level %d is missing from the subtree", i)
		}
	}
}

// The depth bound is a bound, not a suggestion. Roots count as depth 1.
func TestDescend_RespectsTheDepthBound(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	ids := tree(t, ctx, ex, "depth", 4)

	for depth, want := range map[int64]int{1: 1, 2: 2, 3: 3, 4: 4, 99: 4} {
		rows, err := org.Descend(ctx, ex, ids[:1], depth)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != want {
			t.Errorf("maxDepth %d returned %d rows, want %d", depth, len(rows), want)
		}
	}
}

// Ascend walks the other way.
func TestAscend_ReturnsTheAncestorChain(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	ids := tree(t, ctx, ex, "asc", 4)

	rows, err := org.Ascend(ctx, ex, ids[3:], 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want the leaf and three ancestors", len(rows))
	}
	got := map[[16]byte]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	for i, id := range ids {
		if !got[id] {
			t.Errorf("ancestor at level %d is missing", i)
		}
	}
}

// THE GATE. A foreign key does not stop A pointing at B pointing at A, and an
// unguarded recursive query over that does not return. This builds a real cycle
// in a real database and requires the query to terminate.
func TestDescend_DoesNotHangOnACycle(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	ids := tree(t, ctx, ex, "cycle", 3)

	// Close the loop: the root's parent becomes the leaf.
	root, ok, err := org.New().Where(org.ID.Eq(ids[0])).One(ctx, ex)
	if err != nil || !ok {
		t.Fatalf("re-read the root: %v ok=%v", err, ok)
	}
	m := org.Mutate(root)
	m.SetParentID(ids[2])
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Break the cycle before the tree cleanup runs, or the deletes deadlock
		// on each other's references.
		r, ok, _ := org.New().Where(org.ID.Eq(ids[0])).One(ctx, ex)
		if ok {
			mm := org.Mutate(r)
			mm.SetParentIDNull()
			_ = mm.Update(ctx, ex)
		}
	})

	// Prove the cycle actually exists. A test that passes because the update
	// silently failed proves nothing at all.
	back, ok, err := org.New().Where(org.ID.Eq(ids[0])).One(ctx, ex)
	if err != nil || !ok {
		t.Fatalf("re-read after closing the loop: %v ok=%v", err, ok)
	}
	if pid, has := back.ParentID.Get(); !has || pid != ids[2] {
		t.Fatal("the cycle was not created — this test would pass on a plain chain")
	}

	done := make(chan int, 1)
	go func() {
		rows, err := org.Descend(ctx, ex, ids[:1], 1000)
		if err != nil {
			t.Error(err)
			done <- -1
			return
		}
		done <- len(rows)
	}()

	select {
	case n := <-done:
		if n < 0 {
			return
		}
		// Three nodes in a loop, entered at one of them, gives three rows —
		// each node exactly once.
		//
		// The guard excludes the repeating row rather than emitting it and
		// stopping afterwards, so a cycle does not put a duplicate in the
		// result set. That matters: a caller building a map by id would
		// silently overwrite, and a caller counting would be wrong.
		if n != 3 {
			t.Errorf("walked %d rows over a three-node cycle, want each node exactly once", n)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a recursive traversal over a cycle did not terminate — the cycle guard is not working")
	}
}

// An unbounded traversal is the one shape where a missing bound is an outage
// rather than a large result. It must be refused, not defaulted.
func TestDescend_RequiresADepthBound(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	for _, depth := range []int64{0, -1} {
		if _, err := org.Descend(ctx, ex, [][16]byte{{1}}, depth); !errors.Is(err, org.ErrDepth) {
			t.Errorf("maxDepth %d returned %v, want a depth error", depth, err)
		}
	}
}
