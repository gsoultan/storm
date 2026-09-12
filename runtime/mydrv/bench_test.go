package mydrv_test

// What the adapter costs per row, measured against a real server.
//
// The reason storm has a hand-written MySQL adapter at all is an allocation
// number: 8.07 allocs/row through go-sql-driver, 9.07 through vitess, 1.07 for
// a minimal wire client (internal/mysqlspike). Those were measured on a spike.
// This measures the code that actually ships, because a claim about a spike is
// a claim about code nobody runs.

import (
	"context"
	"strconv"
	"testing"

	"github.com/gsoultan/storm/runtime/mydrv"
)

// seedBench fills the probe table.
//
// Ids start at a million on purpose: 1..200 are in Go's preallocated small-int
// range, so a benchmark seeded with them reports allocations that a real
// workload does not get. That already produced a wrong number once.
func seedBench(b *testing.B, c *mydrv.Conn) {
	b.Helper()
	ctx := context.Background()
	must := func(sql string, args []any) {
		if _, err := c.Exec(ctx, sql, args); err != nil {
			b.Fatalf("%s: %v", sql, err)
		}
	}
	must("DROP TABLE IF EXISTS `bench_probe`", nil)
	cols := ""
	for i := 0; i < 8; i++ {
		if i > 0 {
			cols += ", "
		}
		cols += "`c" + strconv.Itoa(i) + "` BIGINT NOT NULL"
	}
	must("CREATE TABLE `bench_probe` (`id` BIGINT PRIMARY KEY, "+cols+") ENGINE=InnoDB", nil)
	for r := 0; r < 200; r++ {
		args := make([]any, 9)
		args[0] = int64(1_000_000 + r)
		for i := 0; i < 8; i++ {
			args[i+1] = int64(1_000_000 + r*8 + i)
		}
		must("INSERT INTO `bench_probe` VALUES (?,?,?,?,?,?,?,?,?)", args)
	}
	b.Cleanup(func() { _, _ = c.Exec(ctx, "DROP TABLE IF EXISTS `bench_probe`", nil) })
}

// 200 rows x 8 columns, the shape the spike used, so the numbers compare.
func BenchmarkQuery200x8(b *testing.B) {
	c, err := mydrv.Open(context.Background(), config(b))
	if err != nil {
		b.Skipf("no server: %v", err)
	}
	defer c.Close()
	ctx := context.Background()
	seedBench(b, c)

	const sql = "SELECT `c0`,`c1`,`c2`,`c3`,`c4`,`c5`,`c6`,`c7` FROM `bench_probe`"
	b.ReportAllocs()
	b.ResetTimer()
	rows := 0
	for i := 0; i < b.N; i++ {
		r, err := c.Query(ctx, sql, nil)
		if err != nil {
			b.Fatal(err)
		}
		for r.Next() {
			v := r.RawValues()
			if len(v) != 8 {
				b.Fatalf("%d columns", len(v))
			}
			rows++
		}
		r.Close()
		if err := r.Err(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	// Per ROW, not per iteration: the spike's numbers are per row and a
	// comparison against a per-query figure would be off by two hundred.
	b.ReportMetric(float64(rows)/float64(b.N), "rows/op")
}
