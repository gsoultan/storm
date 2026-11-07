package bsondec_test

import (
	"testing"

	"github.com/gsoultan/storm/runtime/bsondec"
)

// FuzzDecoders: no element a server sends panics a decoder. Each takes the
// element's type byte followed by its value, which is what generated code
// hands them out of a document.
func FuzzDecoders(f *testing.F) {
	for _, s := range [][]byte{
		{0x10, 1, 0, 0, 0}, {0x12, 1, 0, 0, 0, 0, 0, 0, 0}, {0x01, 0, 0, 0, 0, 0, 0, 0xf0, 0x3f},
		{0x02, 3, 0, 0, 0, 'h', 'i', 0}, {0x05, 2, 0, 0, 0, 0, 'a', 'b'}, {0x08, 1}, {0x0A},
		{0x09, 0, 0, 0, 0, 0, 0, 0, 0}, {0x13, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x40, 0x30},
		{0x03, 5, 0, 0, 0, 0}, {0x04, 13, 0, 0, 0, 0x10, '0', 0, 7, 0, 0, 0, 0},
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = bsondec.Bool(b)
		_, _ = bsondec.Int8(b)
		_, _ = bsondec.Int4(b)
		_, _ = bsondec.Int2(b)
		_, _ = bsondec.Float8(b)
		_, _ = bsondec.Float4(b)
		_, _ = bsondec.Str(b)
		_, _ = bsondec.Bytes(b)
		_, _ = bsondec.UUID(b)
		_, _ = bsondec.Time(b)
		_, _ = bsondec.Date(b)
		_, _ = bsondec.Decimal(b)
		_, _ = bsondec.JSON(b)
		_, _ = bsondec.Nullable(b, bsondec.Int8)
		_, _ = bsondec.Array(b, bsondec.Str)
		_, _ = bsondec.Array(b, bsondec.Decimal)
		if len(b) > 0 {
			_ = bsondec.Size(b[0], b[1:])
			_ = bsondec.Elements(b[1:], func(byte, string, []byte) error { return nil })
		}
	})
}

// A bare type byte is not a JSON value. JSON indexed the fixed-width cases
// without sizing them first, so `\x12` alone — an int64 with no bytes —
// panicked instead of reporting the shape.
func TestJSONSizesTheValueFirst(t *testing.T) {
	for _, b := range [][]byte{{0x12}, {0x10, 1}, {0x08}, {0x01, 0, 0}} {
		if _, err := bsondec.JSON(b); err == nil {
			t.Errorf("%x decoded", b)
		}
	}
}
