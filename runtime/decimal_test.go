package runtime_test

import (
	"errors"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

// Round-tripping through PostgreSQL's own wire format is the only proof that
// matters: base-10000 groups, a weight that can be negative, and a scale that
// need not align to a group boundary are exactly where a hand-rolled codec goes
// wrong.
func TestDecimal_WireRoundTrip(t *testing.T) {
	for _, s := range []string{
		"0", "1", "-1", "0.01", "-0.01", "12345.6789", "-12345.6789",
		"0.1", "0.001", "1000000", "-1000000", "9999.9999",
		"123456789012.12", "-123456789012.12", "0.000001", "10000", "99999999",
	} {
		want, err := runtime.ParseDecimal(s)
		if err != nil {
			t.Fatalf("ParseDecimal(%q): %v", s, err)
		}
		got, err := runtime.DecodeNumeric(runtime.EncodeNumeric(want, nil))
		if err != nil {
			t.Fatalf("%q: decode: %v", s, err)
		}
		if got.String() != want.String() {
			t.Errorf("%q round-tripped to %q (unscaled %d scale %d)",
				s, got.String(), got.Unscaled, got.Scale)
		}
	}
}

func TestDecimal_String(t *testing.T) {
	for _, tc := range []struct {
		d    runtime.Decimal
		want string
	}{
		{runtime.Decimal{Unscaled: 0, Scale: 2}, "0.00"},
		{runtime.Decimal{Unscaled: 1, Scale: 2}, "0.01"},
		{runtime.Decimal{Unscaled: -1, Scale: 2}, "-0.01"},
		{runtime.Decimal{Unscaled: 123456, Scale: 2}, "1234.56"},
		{runtime.Decimal{Unscaled: -123456, Scale: 2}, "-1234.56"},
		{runtime.Decimal{Unscaled: 5, Scale: 0}, "5"},
	} {
		if got := tc.d.String(); got != tc.want {
			t.Errorf("Decimal{%d,%d}.String() = %q, want %q",
				tc.d.Unscaled, tc.d.Scale, got, tc.want)
		}
	}
}

// A value too large to represent must be an ERROR, not a wrong number.
// Silently truncating money is the one behaviour that can never be an option.
func TestDecimal_OverflowIsAnError(t *testing.T) {
	// 30 digits: well past what 18 significant digits can hold.
	huge := make([]byte, 0, 32)
	huge = append(huge,
		0, 8, // ndigits
		0, 7, // weight
		0, 0, // sign +
		0, 0) // dscale
	for i := 0; i < 8; i++ {
		huge = append(huge, 0x27, 0x0F) // 9999
	}
	if _, err := runtime.DecodeNumeric(huge); !errors.Is(err, runtime.ErrDecimalRange) {
		t.Errorf("a 32-digit numeric returned %v, want ErrDecimalRange", err)
	}
	if _, err := runtime.ParseDecimal("123456789012345678901234567890"); !errors.Is(err, runtime.ErrDecimalRange) {
		t.Errorf("parsing a 30-digit literal returned %v, want ErrDecimalRange", err)
	}
}

// NaN has no fixed-point representation, and must not become zero.
func TestDecimal_NaNIsAnError(t *testing.T) {
	nan := []byte{0, 0, 0, 0, 0xC0, 0x00, 0, 0}
	if _, err := runtime.DecodeNumeric(nan); !errors.Is(err, runtime.ErrDecimalNaN) {
		t.Errorf("NaN returned %v, want ErrDecimalNaN", err)
	}
}

func TestDecimal_ShortInputIsAnError(t *testing.T) {
	if _, err := runtime.DecodeNumeric([]byte{0, 1}); err == nil {
		t.Error("a truncated wire value must be an error")
	}
	if _, err := runtime.DecodeNumeric([]byte{0, 4, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Error("a value claiming more digits than it carries must be an error")
	}
}

// Decoding must never panic, whatever the bytes: a wire value is the one input
// the library does not control.
func FuzzDecodeNumeric(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0, 1, 0, 0, 0, 0, 0, 2, 0x27, 0x0F})
	f.Add([]byte{0, 2, 0xFF, 0xFF, 0x40, 0x00, 0, 4, 0x27, 0x0F, 0x00, 0x01})
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := runtime.DecodeNumeric(b)
		if err != nil {
			return
		}
		// A decoded value must render, and re-encode to something that decodes
		// back to the same number.
		_ = d.String()
		again, err := runtime.DecodeNumeric(runtime.EncodeNumeric(d, nil))
		if err != nil {
			t.Fatalf("re-encoding %s failed: %v", d.String(), err)
		}
		if again.String() != d.String() {
			t.Fatalf("%s re-encoded to %s", d.String(), again.String())
		}
	})
}
