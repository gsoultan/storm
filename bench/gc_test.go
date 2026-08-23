package bench

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/raorm/internal/spike"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestGCPressure answers the question single-query wall clock cannot: what does
// each implementation cost the *allocator* for identical work?
//
// Wall clock here is still database-bound, so it is reported but not the point.
// The signal is GC cycles, GC CPU fraction, and total bytes allocated for a
// fixed number of identical queries.

type gcResult struct {
	name       string
	wall       time.Duration
	numGC      uint32
	pauseNs    uint64
	gcCPU      float64
	mallocs    uint64
	totalAlloc uint64
}

func measure(name string, ops, workers int, work func(ctx context.Context, buf []spike.Row) ([]spike.Row, error)) gcResult {
	ctx := context.Background()

	// Settle: two collections, then a pause, so the baseline is clean.
	runtime.GC()
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	var wg sync.WaitGroup
	per := ops / workers
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]spike.Row, 0, gcRows)
			for i := 0; i < per; i++ {
				var err error
				if buf, err = work(ctx, buf[:0]); err != nil {
					panic(err)
				}
			}
		}()
	}
	wg.Wait()
	wall := time.Since(start)

	runtime.ReadMemStats(&after)
	return gcResult{
		name:       name,
		wall:       wall,
		numGC:      after.NumGC - before.NumGC,
		pauseNs:    after.PauseTotalNs - before.PauseTotalNs,
		gcCPU:      after.GCCPUFraction,
		mallocs:    after.Mallocs - before.Mallocs,
		totalAlloc: after.TotalAlloc - before.TotalAlloc,
	}
}

const (
	gcOps     = 4000
	gcWorkers = 16
	gcRows    = 500
)

func TestGCPressure(t *testing.T) {
	if testing.Short() {
		t.Skip("saturation harness")
	}
	const q = cols + ` WHERE status = $1 ORDER BY created_at DESC, id LIMIT $2`
	slow := spike.PgxExec{Pool: pool}
	sq := spike.New().Status("active").Limit(gcRows)
	sq.SQL()

	results := []gcResult{
		measure("pgx Query+Scan", gcOps, gcWorkers, func(ctx context.Context, buf []spike.Row) ([]spike.Row, error) {
			rows, err := pool.Query(ctx, q, "active", gcRows)
			if err != nil {
				return buf, err
			}
			for rows.Next() {
				buf = append(buf, spike.Row{})
				r := &buf[len(buf)-1]
				var age pgtype.Int4
				if err := rows.Scan(&r.ID, &r.OrgID, &r.Email, &r.Name, &age,
					&r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
					rows.Close()
					return buf, err
				}
				r.Age = spike.Null[int32]{V: age.Int32, Valid: age.Valid}
			}
			rows.Close()
			return buf, rows.Err()
		}),
		measure("raorm (slab)", gcOps, gcWorkers, func(ctx context.Context, buf []spike.Row) ([]spike.Row, error) {
			var sl spike.Slab
			return sq.AllInto(ctx, slow, buf, &sl)
		}),
	}

	fmt.Printf("\n=== GC pressure: %d queries x %d rows, %d workers ===\n", gcOps, gcRows, gcWorkers)
	fmt.Printf("%-18s %10s %8s %12s %14s %12s\n", "impl", "wall", "GCs", "pause", "mallocs", "alloc MiB")
	for _, r := range results {
		fmt.Printf("%-18s %10s %8d %12s %14d %12.1f\n",
			r.name, r.wall.Round(time.Millisecond), r.numGC,
			time.Duration(r.pauseNs).Round(time.Microsecond),
			r.mallocs, float64(r.totalAlloc)/(1<<20))
	}
	a, b := results[0], results[1]
	fmt.Printf("\nraorm vs pgx: %.1fx fewer mallocs, %.1fx less allocated, %.1fx fewer GCs\n\n",
		float64(a.mallocs)/float64(b.mallocs),
		float64(a.totalAlloc)/float64(b.totalAlloc),
		float64(a.numGC)/float64(max(b.numGC, 1)))
}

func max(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}
