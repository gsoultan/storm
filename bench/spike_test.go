package bench

import (
	"context"
	"testing"
	"time"

	"github.com/gsoultan/raorm/internal/spike"
)

func exec() spike.PgxExec { return spike.PgxExec{Pool: pool} }

// dynamic6 is the query under test: all six optional filters set.
func dynamic6() spike.Query {
	return spike.New().
		Org(orgs[3]). // user000003: org 3, age 21 (not null), status "active"
		Email("user000003@corp.com").
		NameLike("User %").
		AgeGte(21).
		Status("active").
		Since(time.Now().Add(-400 * 24 * time.Hour)).
		Limit(50)
}

// ---- the claim that needs no database: warm dynamic construction ----

func BenchmarkPrepare_Warm(b *testing.B) {
	q := dynamic6()
	q.SQL() // warm the shape
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bd := spike.GetBinder()
		sql, args := q.Prepare(bd)
		if len(sql) == 0 || len(args) != 7 {
			b.Fatal("bad prepare")
		}
		spike.PutBinder(bd)
	}
}

// Building the query from scratch each time, the way request code does.
func BenchmarkBuildAndPrepare_Warm(b *testing.B) {
	dynamic6().SQL()
	now := time.Now().Add(-400 * 24 * time.Hour)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := spike.New().
			Org(orgs[3]).Email("user000042@corp.com").NameLike("User %").
			AgeGte(21).Status("active").Since(now).Limit(50)
		bd := spike.GetBinder()
		_, args := q.Prepare(bd)
		if len(args) != 7 {
			b.Fatal("bad prepare")
		}
		spike.PutBinder(bd)
	}
}

// Cold path: what compiling one shape costs, paid once per shape per process.
func BenchmarkCompile_Cold(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if spike.CompileForTest(uint32(i%spike.NumShapes)) == "" {
			b.Fatal("empty")
		}
	}
}

// Total cost of warming every shape this table can produce.
func BenchmarkCompile_AllShapes(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for m := uint32(0); m < spike.NumShapes; m++ {
			_ = spike.CompileForTest(m)
		}
	}
}

// ---- end to end ----

func BenchmarkGet_Spike(b *testing.B) {
	ctx, ex := context.Background(), exec()
	var r spike.Row
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sl spike.Slab
		ok, err := spike.Get(ctx, ex, ids[i%len(ids)], &r, &sl)
		if err != nil || !ok {
			b.Fatal(err, ok)
		}
	}
}

func BenchmarkScan1000_Spike(b *testing.B) {
	ctx, ex := context.Background(), exec()
	q := spike.New().Status("active").Limit(1000)
	q.SQL()
	buf := make([]spike.Row, 0, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		buf, err = q.All(ctx, ex, buf[:0])
		if err != nil || len(buf) != 1000 {
			b.Fatal(err, len(buf))
		}
	}
}

func BenchmarkDynamic6_Spike(b *testing.B) {
	ctx, ex := context.Background(), exec()
	q := dynamic6()
	q.SQL()
	buf := make([]spike.Row, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		buf, err = q.All(ctx, ex, buf[:0])
		if err != nil {
			b.Fatal(err)
		}
	}
}

// ---- pgconn fast path: no []any, no type map, no Scan ----

func BenchmarkGet_Fast(b *testing.B) {
	ctx := context.Background()
	ex := spike.FastExec{Pool: pool}
	var r spike.Row
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sl spike.Slab
		ok, err := spike.GetFast(ctx, ex, ids[i%len(ids)], &r, &sl)
		if err != nil || !ok {
			b.Fatal(err, ok)
		}
	}
}

func BenchmarkScan1000_Fast(b *testing.B) {
	ctx := context.Background()
	ex := spike.FastExec{Pool: pool}
	q := spike.New().Status("active").Limit(1000)
	buf := make([]spike.Row, 0, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sl spike.Slab
		var err error
		if buf, err = q.AllFast(ctx, ex, buf[:0], &sl); err != nil {
			b.Fatal(err)
		}
		if len(buf) != 1000 {
			b.Fatal(len(buf))
		}
	}
}

func BenchmarkDynamic6_Fast(b *testing.B) {
	ctx := context.Background()
	ex := spike.FastExec{Pool: pool}
	q := dynamic6()
	buf := make([]spike.Row, 0, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sl spike.Slab
		var err error
		if buf, err = q.AllFast(ctx, ex, buf[:0], &sl); err != nil {
			b.Fatal(err)
		}
	}
}
