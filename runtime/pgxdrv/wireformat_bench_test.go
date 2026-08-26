package pgxdrv

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The guard's whole cost: one pass over a result's field descriptors, once
// per statement. Eight columns is a normal generated Row.
func BenchmarkCheckFormats(b *testing.B) {
	fds := []pgconn.FieldDescription{
		{Name: "id", DataTypeOID: 2950, Format: 1},
		{Name: "created_at", DataTypeOID: 1184, Format: 1},
		{Name: "updated_at", DataTypeOID: 1184, Format: 1},
		{Name: "tenant_id", DataTypeOID: 2950, Format: 1},
		{Name: "is_system", DataTypeOID: 16, Format: 1},
		{Name: "name", DataTypeOID: 25, Format: 0},
		{Name: "description", DataTypeOID: 25, Format: 0},
		{Name: "kinds", DataTypeOID: 1009, Format: 1},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range fds {
			if !formatOK(fds[j].DataTypeOID, fds[j].Format) {
				b.Fatal("fixture must pass")
			}
		}
	}
}
