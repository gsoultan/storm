package bench

import (
	"context"
	"testing"

	"github.com/gsoultan/raorm/internal/spike"
)

// captureRaw pulls one row's wire bytes so the decoder can be benchmarked
// without the 64 µs network round trip masking it.
func captureRaw(tb testing.TB) [][]byte {
	rows, err := pool.Query(context.Background(), cols+` WHERE id = $1`, ids[3])
	if err != nil {
		tb.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		tb.Fatal("no row")
	}
	src := rows.RawValues()
	out := make([][]byte, len(src))
	for i, b := range src {
		if b != nil {
			out[i] = append([]byte(nil), b...)
		}
	}
	return out
}

// BenchmarkDecodeRow_Offline is raorm's whole scan cost for an 8-column row:
// no driver, no network, just wire bytes to struct.
func BenchmarkDecodeRow_Offline(b *testing.B) {
	rv := captureRaw(b)
	var r spike.Row
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sl spike.Slab
		spike.DecodeRowForTest(rv, &r, &sl)
	}
}

// BenchmarkDecode1000_Slab is the arena win in isolation: 1,000 rows decoded
// into one Slab instead of 3,000 individual string allocations.
func BenchmarkDecode1000_Slab(b *testing.B) {
	rv := captureRaw(b)
	dst := make([]spike.Row, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var sl spike.Slab
		for j := range dst {
			spike.DecodeRowForTest(rv, &dst[j], &sl)
		}
	}
}

// BenchmarkDecode1000_PerRowString is the same work with plain string(), the
// shape every ORM ships today.
func BenchmarkDecode1000_PerRowString(b *testing.B) {
	rv := captureRaw(b)
	dst := make([]spike.Row, 1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range dst {
			r := &dst[j]
			copy(r.ID[:], rv[0])
			copy(r.OrgID[:], rv[1])
			r.Email = string(rv[2])
			r.Name = string(rv[3])
			r.Status = string(rv[5])
		}
	}
}
