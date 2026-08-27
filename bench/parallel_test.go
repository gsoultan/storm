package bench

import (
	"context"
	"testing"

	"github.com/gsoultan/storm/internal/spike"
	"github.com/jackc/pgx/v5/pgtype"
)

// Single-query wall clock is dominated by the network, so it cannot separate
// these implementations. Under concurrency, allocation count turns into GC
// work, and GC work turns into throughput. That is where an ORM can actually
// win — so this is the benchmark that decides "fastest".

const scanN = 100

func BenchmarkParallel_Scan100_Pgx(b *testing.B) {
	ctx := context.Background()
	const q = cols + ` WHERE status = $1 ORDER BY created_at DESC, id LIMIT $2`
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]spike.Row, 0, scanN)
		for pb.Next() {
			rows, err := pool.Query(ctx, q, "active", scanN)
			if err != nil {
				b.Error(err)
				return
			}
			buf = buf[:0]
			for rows.Next() {
				buf = append(buf, spike.Row{})
				r := &buf[len(buf)-1]
				var age pgtype.Int4
				if err := rows.Scan(&r.ID, &r.OrgID, &r.Email, &r.Name, &age,
					&r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
					rows.Close()
					b.Error(err)
					return
				}
				r.Age = spike.Null[int32]{V: age.Int32, Valid: age.Valid}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkParallel_Scan100_Spike(b *testing.B) {
	ctx := context.Background()
	ex := spike.PgxExec{Pool: pool}
	q := spike.New().Status("active").Limit(scanN)
	q.SQL()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]spike.Row, 0, scanN)
		for pb.Next() {
			var sl spike.Slab
			var err error
			if buf, err = q.AllInto(ctx, ex, buf[:0], &sl); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkParallel_Scan100_Fast(b *testing.B) {
	ctx := context.Background()
	ex := spike.FastExec{Pool: pool}
	q := spike.New().Status("active").Limit(scanN)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]spike.Row, 0, scanN)
		for pb.Next() {
			var sl spike.Slab
			var err error
			if buf, err = q.AllFast(ctx, ex, buf[:0], &sl); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
