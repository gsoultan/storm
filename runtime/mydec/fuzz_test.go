package mydec_test

import (
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/mydec"
)

// FuzzDecoders: no value a MySQL server sends panics a decoder. The fallible
// ones read a leading length that the server sets.
func FuzzDecoders(f *testing.F) {
	for _, s := range [][]byte{
		{}, {0}, {4, 0xe8, 0x07, 1, 2}, {7, 0xe8, 0x07, 1, 2, 3, 4, 5},
		{11, 0xe8, 0x07, 1, 2, 3, 4, 5, 1, 0, 0, 0}, {8, 0, 1, 0, 0, 0, 2, 3, 4},
		{12, 1, 1, 0, 0, 0, 2, 3, 4, 1, 0, 0, 0}, []byte("-123.4500"), []byte("1e5"),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var sl runtime.Slab
		_ = mydec.Bool(b)
		_ = mydec.Int1(b)
		_ = mydec.Int2(b)
		_ = mydec.Int4(b)
		_ = mydec.Int8(b)
		_ = mydec.Float4(b)
		_ = mydec.Float8(b)
		_ = mydec.Bytes(b)
		_ = mydec.UUID(b)
		_, _ = mydec.DateTime(b)
		_, _ = mydec.Date(b)
		_, _ = mydec.Duration(b)
		_, _ = mydec.Decimal(b)
		_, _ = mydec.TimeOfDay(b)
		_ = mydec.NullText(b, &sl)
		_ = mydec.NullJSON(b, &sl)
		_ = mydec.JSONB(b, &sl)
		_, _ = mydec.NullNumeric(b)
		_, _ = mydec.NullDateTime(b)
		_, _ = mydec.NullDuration(b)
		_, _ = mydec.NullTimeOfDay(b)
	})
}
