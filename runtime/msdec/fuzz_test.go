package msdec_test

import (
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/msdec"
)

// FuzzDecoders: no value panics a SQL Server decoder, whatever its length.
func FuzzDecoders(f *testing.F) {
	for _, s := range [][]byte{
		{}, {1}, {2, 0, 0, 0, 0, 0, 0, 0, 0}, {0x68, 0, 0x69, 0}, {0x3d, 0xd8, 0x29, 0xdf},
		{0, 0, 0, 0, 0, 0, 0, 0}, {0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, {1, 2, 3},
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var sl runtime.Slab
		_ = msdec.Bool(b)
		_ = msdec.Int1(b)
		_ = msdec.Int2(b)
		_ = msdec.Int4(b)
		_ = msdec.Int8(b)
		_ = msdec.Float4(b)
		_ = msdec.Float8(b)
		_ = msdec.Str(b, &sl)
		_ = msdec.Bytes(b, &sl)
		_ = msdec.UUID(b)
		_, _ = msdec.DateTimeOffset(b)
		_, _ = msdec.DateTime(b)
		_, _ = msdec.Date(b)
		_, _ = msdec.TimeOfDay(b)
		_, _ = msdec.Decimal(b)
		_ = msdec.JSONB(b, &sl)
		_ = msdec.NullText(b, &sl)
		_, _ = msdec.NullNumeric(b)
		_, _ = msdec.NullDateTimeOffset(b)
		_, _ = msdec.NullDateTime(b)
		_, _ = msdec.NullDate(b)
		_, _ = msdec.NullTimeOfDay(b)
	})
}
