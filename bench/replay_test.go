package bench

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gsoultan/raorm/internal/spike"
)

// The GC harness shows cost; this shows the ceiling. With the database removed
// from the loop, the only remaining work is decode + allocate, so throughput
// here is the limit raorm imposes on an application no matter how fast the
// database gets.

type replayRows struct {
	src [][][]byte
	i   int
}

func (r *replayRows) Next() bool          { r.i++; return r.i <= len(r.src) }
func (r *replayRows) RawValues() [][]byte { return r.src[r.i-1] }
func (r *replayRows) Close()              { r.i = 0 }
func (r *replayRows) Err() error          { return nil }

type replayExec struct{ src [][][]byte }

func (e replayExec) Query(context.Context, string, []any) (spike.Rows, error) {
	return &replayRows{src: e.src}, nil
}

// captureRows pulls n real rows off the wire once, so the replay decodes the
// same bytes Postgres actually sends.
func captureRows(tb testing.TB, n int) [][][]byte {
	rows, err := pool.Query(context.Background(),
		cols+` WHERE status = $1 ORDER BY created_at DESC, id LIMIT $2`, "active", n)
	if err != nil {
		tb.Fatal(err)
	}
	defer rows.Close()
	var out [][][]byte
	for rows.Next() {
		src := rows.RawValues()
		row := make([][]byte, len(src))
		for i, b := range src {
			if b != nil {
				row[i] = append([]byte(nil), b...)
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		tb.Fatal(err)
	}
	if len(out) != n {
		tb.Fatalf("captured %d rows, want %d", len(out), n)
	}
	return out
}

// TestClientCeiling saturates the client with no database in the way.
func TestClientCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("saturation harness")
	}
	const (
		rows    = 500
		queries = 40_000
		workers = 16
	)
	src := captureRows(t, rows)
	ex := replayExec{src: src}
	q := spike.New().Status("active").Limit(rows)
	q.SQL()

	type impl struct {
		name string
		run  func(buf []spike.Row) []spike.Row
	}
	impls := []impl{
		{"raorm (slab)", func(buf []spike.Row) []spike.Row {
			var sl spike.Slab
			out, err := q.AllInto(context.Background(), ex, buf, &sl)
			if err != nil {
				panic(err)
			}
			return out
		}},
		{"per-row string()", func(buf []spike.Row) []spike.Row {
			for _, rv := range src {
				buf = append(buf, spike.Row{})
				r := &buf[len(buf)-1]
				copy(r.ID[:], rv[0])
				copy(r.OrgID[:], rv[1])
				r.Email = string(rv[2])
				r.Name = string(rv[3])
				r.Status = string(rv[5])
			}
			return buf
		}},
	}

	fmt.Printf("\n=== client ceiling: no database, %d queries x %d rows, %d workers ===\n",
		queries, rows, workers)
	fmt.Printf("%-20s %10s %14s %12s %8s %12s\n",
		"impl", "wall", "rows/sec", "mallocs", "GCs", "alloc MiB")

	var base float64
	for _, im := range impls {
		runtime.GC()
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)

		var done atomic.Int64
		start := time.Now()
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				buf := make([]spike.Row, 0, rows)
				for i := 0; i < queries/workers; i++ {
					buf = im.run(buf[:0])
					done.Add(int64(len(buf)))
				}
			}()
		}
		wg.Wait()
		wall := time.Since(start)
		runtime.ReadMemStats(&after)

		rps := float64(done.Load()) / wall.Seconds()
		if base == 0 {
			base = rps
		}
		fmt.Printf("%-20s %10s %14s %12d %8d %12.1f\n",
			im.name, wall.Round(time.Millisecond),
			fmt.Sprintf("%.1fM", rps/1e6),
			after.Mallocs-before.Mallocs, after.NumGC-before.NumGC,
			float64(after.TotalAlloc-before.TotalAlloc)/(1<<20))
	}
	fmt.Println()
}
