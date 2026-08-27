package bench

import (
	"testing"

	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgtype"
)

// int8[] and text[] on the `= ANY($1)` path, the same measurement that earned
// uuid[] its codec.
//
// This was written BEFORE the codecs existed, to decide whether they were
// justified: uuid[] cost 1,003 allocations for 500 ids, and the question was
// whether the other two key types cost anything like it. They did — about one
// allocation per element — so the codecs were written. The generic arms stay
// so the comparison keeps being made rather than remembered.
//
// int8[] matters more than the fixture suggests: storm's own schema is
// uuid-keyed, but most Postgres schemas that are not uuid-first use bigserial
// primary keys, and every relation load in one of those binds int8[].
func BenchmarkEncodeInt8Array(b *testing.B) {
	m := pgtype.NewMap()
	const int8ArrayOID = 1016
	const textArrayOID = 1009

	ids := make([]int64, 500)
	for i := range ids {
		ids[i] = int64(i) * 7
	}
	strs := make([]string, 500)
	for i := range strs {
		strs[i] = "key-000000" + string(rune('a'+i%26))
	}

	b.Run("int8_generic", func(b *testing.B) {
		buf := make([]byte, 0, 1<<16)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := m.Encode(int8ArrayOID, pgtype.BinaryFormatCode, ids, buf[:0]); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("int8_storm", func(b *testing.B) {
		fm := pgtype.NewMap()
		pgxdrv.RegisterFastArrays(fm)
		buf := make([]byte, 0, 1<<16)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := fm.Encode(int8ArrayOID, pgtype.BinaryFormatCode, ids, buf[:0]); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("text_generic", func(b *testing.B) {
		buf := make([]byte, 0, 1<<16)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := m.Encode(textArrayOID, pgtype.BinaryFormatCode, strs, buf[:0]); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("text_storm", func(b *testing.B) {
		fm := pgtype.NewMap()
		pgxdrv.RegisterFastArrays(fm)
		buf := make([]byte, 0, 1<<16)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := fm.Encode(textArrayOID, pgtype.BinaryFormatCode, strs, buf[:0]); err != nil {
				b.Fatal(err)
			}
		}
	})
}
