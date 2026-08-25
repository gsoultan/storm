package planspike_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/gsoultan/raorm/internal/planspike/store"
	"github.com/gsoultan/raorm/internal/planspike/store/user"
	raormrt "github.com/gsoultan/raorm/runtime"
)

// heapNow forces collection and reports live heap. Two GCs: the first turns
// unreachable into collectable, the second collects what finalization freed.
func heapNow() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// A pooled binder must not PIN the caller's memory. It used to: binding an
// In(...) copied the caller's slice reference into the binder, and binding a
// string copied its header — so a binder sitting idle in the pool kept a
// 500-id array and every bound string alive indefinitely, per slot, per
// table. This drives fresh half-megabyte slices through the bind path and
// requires the heap to come back down; with the old behaviour the pool
// retains ~32MB here and the assertion fails by an order of magnitude.
func TestBinderPool_DoesNotPinCallerMemory(t *testing.T) {
	ctx := context.Background()
	// A STUB executor, deliberately: against the live driver this test
	// measures pgx's per-connection write buffers, which grow to fit the
	// 640KB encoded payload and stay grown — real, bounded, and not ours.
	// The binder pool pins or does not pin regardless of what executes.

	// Pinning scales with POOL OCCUPANCY, not iterations: a sequential loop
	// reuses one binder and overwrites its stale reference each cycle, so the
	// first version of this test could not fail even without the fix. A
	// CONCURRENT burst is what fills the pool — 64 goroutines leave up to 64
	// binders idle, each still referencing its caller's half-megabyte slice.
	// Occupancy must be FORCED, not hoped for: with an instant executor the
	// goroutines run almost sequentially and keep reusing one binder, so the
	// pool never fills and the unfixed code passes — the third way this test
	// failed to fail. The barrier holds every goroutine INSIDE its query until
	// all 64 binders are simultaneously live; on release all 64 go back to
	// the pool together, and unfixed each still references its 512KB slice.
	burst := func() {
		arrived := make(chan struct{}, 64)
		release := make(chan struct{})
		ex := drainExec{arrived: arrived, release: release}
		var wg sync.WaitGroup
		for g := 0; g < 64; g++ {
			wg.Add(1)
			go func(seed byte) {
				defer wg.Done()
				ids := make([][16]byte, 32*1024) // 512KB, freshly allocated
				for j := range ids {
					ids[j][0] = seed
				}
				if _, err := user.New().Where(user.ID.In(ids...)).Count(ctx, ex); err != nil {
					t.Error(err)
				}
			}(byte(g))
		}
		for i := 0; i < 64; i++ {
			<-arrived
		}
		close(release)
		wg.Wait()
	}
	// The failure mode is a retention CEILING, not a ramp: later bursts
	// replace the pinned slices rather than adding to them, so measuring
	// growth between bursts passes even unfixed — which the first two
	// versions of this test did, and is why its baseline must be taken
	// BEFORE the pool has ever pinned anything. Warm the statement caches
	// with a one-id query (a 16-byte pin), then measure what one burst
	// leaves behind after the load is GONE: that is the memory an idle
	// service holds for work it already finished.
	if _, err := user.New().Where(user.ID.In([16]byte{1})).Count(ctx, drainExec{}); err != nil {
		t.Fatal(err)
	}
	base := heapNow()

	burst()
	retained := int64(heapNow()) - int64(base)
	if retained > 8<<20 {
		t.Fatalf("an idle pool retains %d bytes of finished callers' data after one "+
			"burst — unfixed this reads ~32MB (64 slots × 512KB)", retained)
	}
}

// The broader soak: a mixed read workload, sampled in windows. Retained heap
// may wobble with allocator noise; it must not RAMP with work done — that
// slope is the definition of a leak.
func TestSoak_RetainedHeapDoesNotRampWithWork(t *testing.T) {
	if testing.Short() {
		t.Skip("soak")
	}
	ctx := context.Background()
	ex, _ := db(t)

	window := func() {
		for i := 0; i < 50; i++ {
			email := fmt.Sprintf("soak-%d@example.com", i)
			if _, err := user.New().
				Where(user.Status.Eq("pending"), user.Email.NotEq(email)).
				Limit(20).All(ctx, ex, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := user.New().Where(user.Email.Eq(email)).Exists(ctx, ex); err != nil {
				t.Fatal(err)
			}
			if _, err := user.New().Limit(5).AllContact(ctx, ex); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.OrgWithUsers().Limit(5).All(ctx, ex); err != nil {
			t.Fatal(err)
		}
	}

	window() // warm
	first := heapNow()
	var last uint64
	for w := 0; w < 5; w++ {
		window()
		last = heapNow()
	}
	if grew := int64(last) - int64(first); grew > 2<<20 {
		t.Fatalf("retained heap ramped %d bytes across five identical windows — work is leaking", grew)
	}
}

// drainExec answers every query with zero rows and no driver. Its barrier
// holds callers inside Query so the test controls how many binders are live
// at once — pool occupancy is the variable under test, not scheduler luck.
type drainExec struct {
	arrived chan struct{}
	release chan struct{}
}

type noRows struct{}

func (noRows) Next() bool          { return false }
func (noRows) RawValues() [][]byte { return nil }
func (noRows) Close()              {}
func (noRows) Err() error          { return nil }

func (d drainExec) Query(context.Context, string, []any) (raormrt.Rows, error) {
	if d.arrived != nil {
		d.arrived <- struct{}{}
		<-d.release
	}
	return noRows{}, nil
}
func (drainExec) Exec(context.Context, string, []any) (int64, error) { return 0, nil }
func (drainExec) CopyFrom(context.Context, string, []string, raormrt.CopySource) (int64, error) {
	return 0, nil
}
func (drainExec) Batch(context.Context, []raormrt.BatchOp, func(int, raormrt.Rows, int64, error) error) error {
	return nil
}
