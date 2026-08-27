package bench

import (
	"testing"

	"github.com/gsoultan/storm/runtime/pgxdrv"

	"github.com/jackc/pgx/v5/pgtype"
)

// Where the `= ANY($1)` parameter cost actually is.
//
// A relation load batches children with `= ANY($1)`, so an allocation per bound
// id is an allocation per parent — and the plan layer makes that the common
// case rather than a curiosity. BenchmarkAnyParam measures it end to end; this
// isolates the encoder, with no server and no scan, so the number cannot be
// blamed on anything else.
//
// NEGATIVE RESULT, do not re-try it: pgtype.FlatArray does NOT help. It is
// pgx's own array wrapper and the obvious first thing to reach for, and it
// comes out within noise — both shapes cost ~2 allocations per element, inside
// pgx's generic array codec, which boxes every element into an `any` and builds
// a per-element encode plan.
//
// The fix is therefore not a different pgx wrapper. It is a Codec registered on
// the connection's type map in runtime/pgxdrv that encodes uuid[] straight into
// the output buffer: ndim, hasnull, element oid, one dimension, then 16 bytes
// per id. That is confined to the adapter, which is the only package allowed to
// name a pgx type at all.
func BenchmarkEncodeIDArray(b *testing.B) {
	m := pgtype.NewMap()
	ids := make([][16]byte, 500)
	const uuidArrayOID = 2951

	b.Run("slice_of_array", func(b *testing.B) {
		buf := make([]byte, 0, 1<<16)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := m.Encode(uuidArrayOID, pgtype.BinaryFormatCode, ids, buf[:0]); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("storm_codec", func(b *testing.B) {
		fm := pgtype.NewMap()
		pgxdrv.RegisterFastArrays(fm)
		buf := make([]byte, 0, 1<<16)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := fm.Encode(uuidArrayOID, pgtype.BinaryFormatCode, ids, buf[:0]); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("flat_array", func(b *testing.B) {
		fa := pgtype.FlatArray[[16]byte](ids)
		buf := make([]byte, 0, 1<<16)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := m.Encode(uuidArrayOID, pgtype.BinaryFormatCode, fa, buf[:0]); err != nil {
				b.Fatal(err)
			}
		}
	})
}
