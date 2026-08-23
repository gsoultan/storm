package bench

import (
	"context"
	"testing"
)

// BenchmarkFloor_Ping is the network round trip and nothing else. Every
// end-to-end number in this suite is bounded below by it.
func BenchmarkFloor_Ping(b *testing.B) {
	ctx := context.Background()
	var n int32
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFloor_PgxNoDecode issues the PK query and walks the result without
// decoding anything. This is pgx's own allocation floor: no ORM, however
// perfect, can go below it while using pgx.Query.
func BenchmarkFloor_PgxNoDecode(b *testing.B) {
	ctx := context.Background()
	const q = cols + ` WHERE id = $1`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := pool.Query(ctx, q, ids[i%len(ids)])
		if err != nil {
			b.Fatal(err)
		}
		for rows.Next() {
			_ = rows.RawValues()
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			b.Fatal(err)
		}
	}
}
