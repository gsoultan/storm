package bench

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/storm/internal/spike"
)

// TestConcurrentShapes hammers the compiled-statement cache and the binder pool
// from many goroutines across all 64 shapes at once. The cache is a CAS on an
// atomic.Pointer array and the binder is a sync.Pool; both are shared state on
// the hot path, so both need -race to say anything.
func TestConcurrentShapes(t *testing.T) {
	spike.ResetCacheForTest()
	ctx := context.Background()
	ex := spike.PgxExec{Pool: pool}
	since := time.Now().Add(-400 * 24 * time.Hour)

	const goroutines = 32
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			buf := make([]spike.Row, 0, 32)
			for i := 0; i < 200; i++ {
				mask := uint32((g*7 + i) % spike.NumShapes)
				q := spike.New().Limit(10)
				if mask&1 != 0 {
					q = q.Org(orgs[3])
				}
				if mask&2 != 0 {
					q = q.Email("user000003@corp.com")
				}
				if mask&4 != 0 {
					q = q.NameLike("User 0000%")
				}
				if mask&8 != 0 {
					q = q.AgeGte(21)
				}
				if mask&16 != 0 {
					q = q.Status("active")
				}
				if mask&32 != 0 {
					q = q.Since(since)
				}
				var err error
				if buf, err = q.All(ctx, ex, buf[:0]); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Every shape must have compiled to exactly one distinct statement, and a
	// lost CAS race must not leave a shape unusable.
	seen := map[uint32]string{}
	for m := uint32(0); m < spike.NumShapes; m++ {
		q := spike.New()
		sql := q.SQLForShape(m)
		if sql == "" {
			t.Fatalf("shape %d compiled to empty SQL", m)
		}
		if prev, ok := seen[m]; ok && prev != sql {
			t.Fatalf("shape %d unstable: %q vs %q", m, prev, sql)
		}
		seen[m] = sql
	}
}
