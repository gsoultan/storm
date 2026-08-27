package runtime_test

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// The wire decoders, exercised directly.
//
// They are covered indirectly by every database test, but only for the types
// the fixture happens to use — bool, the float widths and bytea are decoded by
// nobody. A decoder nothing reads is a decoder nothing checks, and these are
// the functions where an endianness or width mistake produces a plausible wrong
// number rather than a failure.
func TestDecoders(t *testing.T) {
	// Convert at run time, not as constants: a hand-written two's-complement
	// hex literal is exactly the kind of thing this test exists to catch, and
	// getting it wrong here would test the mistake instead of the decoder.
	be16 := func(v int16) []byte { return binary.BigEndian.AppendUint16(nil, uint16(v)) }
	be32 := func(v int32) []byte { return binary.BigEndian.AppendUint32(nil, uint32(v)) }
	be64 := func(v int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(v)) }
	beF32 := func(v float32) []byte { return binary.BigEndian.AppendUint32(nil, math.Float32bits(v)) }
	beF64 := func(v float64) []byte { return binary.BigEndian.AppendUint64(nil, math.Float64bits(v)) }

	if !runtime.Bool([]byte{1}) || runtime.Bool([]byte{0}) {
		t.Error("Bool decodes the wrong way round")
	}
	if got := runtime.Int2(be16(-2)); got != -2 {
		t.Errorf("Int2 = %d, want -2", got)
	}
	if got := runtime.Int4(be32(-70000)); got != -70000 {
		t.Errorf("Int4 = %d, want -70000", got)
	}
	if got := runtime.Int8(be64(-5_000_000_000)); got != -5_000_000_000 {
		t.Errorf("Int8 = %d, want -5000000000", got)
	}
	if got := runtime.Float4(beF32(-1.5)); got != -1.5 {
		t.Errorf("Float4 = %v, want -1.5", got)
	}
	if got := runtime.Float8(beF64(-1.25)); got != -1.25 {
		t.Errorf("Float8 = %v, want -1.25", got)
	}

	// Bytes must COPY: the driver reuses its read buffer, so retaining the
	// slice it handed us would alias the next row.
	src := []byte{1, 2, 3}
	got := runtime.Bytes(src)
	src[0] = 9
	if got[0] != 1 {
		t.Error("Bytes aliased the driver's buffer instead of copying it")
	}
	if runtime.Bytes(nil) != nil {
		t.Error("Bytes(nil) should stay nil")
	}

	// Postgres timestamps are microseconds since 2000-01-01.
	if got := runtime.Timestamptz(be64(0)); !got.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Timestamptz(0) = %v, want the 2000 epoch", got)
	}
}

func TestNullDecoders(t *testing.T) {
	var sl runtime.Slab

	if n := runtime.NullText(nil, &sl); n.Valid {
		t.Error("NullText(nil) should be invalid")
	}
	if n := runtime.NullText([]byte("x"), &sl); !n.Valid || n.V != "x" {
		t.Errorf("NullText = %+v", n)
	}
	if n := runtime.NullJSON(nil, &sl); n.Valid {
		t.Error("NullJSON(nil) should be invalid")
	}
	if n, err := runtime.NullNumeric(nil); err != nil || n.Valid {
		t.Errorf("NullNumeric(nil) = %+v, %v", n, err)
	}
	if _, err := runtime.NumericErr(nil); err != nil {
		t.Errorf("NumericErr(nil) should not error: %v", err)
	}

	// Ptr yields a nil *T for SQL NULL, which is what a bulk load binds.
	var absent runtime.Null[int64]
	if p := absent.Ptr(); p != nil {
		t.Error("Ptr on an invalid Null should be nil")
	}
	v := runtime.Null[int64]{V: 7, Valid: true}
	if p := v.Ptr(); p == nil || *p != 7 {
		t.Errorf("Ptr = %v", p)
	}
	if a := v.Arg(); a != int64(7) {
		t.Errorf("Arg = %v", a)
	}
	if a := (runtime.Null[int64]{}).Arg(); a != nil {
		t.Errorf("Arg on an invalid Null = %v, want nil", a)
	}
}

// jsonb arrives with a one-byte version prefix. Handing it to the caller makes
// every unmarshal fail; stripping it in the caller makes every caller know the
// wire format.
func TestJSONB_StripsTheVersionPrefix(t *testing.T) {
	var sl runtime.Slab
	got := runtime.JSONB(append([]byte{1}, []byte(`{"a":1}`)...), &sl)
	if string(got) != `{"a":1}` {
		t.Errorf("JSONB = %q, want the document without its version byte", got)
	}
	if runtime.JSONB(nil, &sl) != nil {
		t.Error("JSONB(nil) should stay nil")
	}
	// Text format has no prefix, and must not lose its first byte.
	if got := runtime.JSONB([]byte(`{"a":1}`), &sl); string(got) != `{"a":1}` {
		t.Errorf("JSONB without a prefix = %q", got)
	}
}

func TestJSON_Unmarshal(t *testing.T) {
	var out struct {
		A int `json:"a"`
	}
	if err := runtime.JSON(`{"a":3}`).Unmarshal(&out); err != nil || out.A != 3 {
		t.Errorf("Unmarshal: %v, out=%+v", err, out)
	}
	// Empty is not an error: a NULL jsonb column leaves the target untouched
	// rather than failing the whole row.
	if err := runtime.JSON(nil).Unmarshal(&out); err != nil {
		t.Errorf("Unmarshal(nil): %v", err)
	}
	if err := runtime.JSON(`{`).Unmarshal(&out); err == nil {
		t.Error("malformed JSON must be an error")
	}
	if s := runtime.JSON(`{"a":1}`).String(); s != `{"a":1}` {
		t.Errorf("String = %q", s)
	}
}

// Float64 is lossy by construction and named so that using it for money is a
// visible decision. It still has to be right.
func TestDecimal_Float64(t *testing.T) {
	for _, tc := range []struct {
		d    runtime.Decimal
		want float64
	}{
		{runtime.Decimal{Unscaled: 12345, Scale: 2}, 123.45},
		{runtime.Decimal{Unscaled: -12345, Scale: 2}, -123.45},
		{runtime.Decimal{Unscaled: 5, Scale: 0}, 5},
		{runtime.Decimal{Unscaled: 5, Scale: -2}, 500},
	} {
		if got := tc.d.Float64(); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("Decimal{%d,%d}.Float64() = %v, want %v",
				tc.d.Unscaled, tc.d.Scale, got, tc.want)
		}
	}
}

// Numeric swallows the error and returns zero; NumericErr reports it. The
// generated scanner uses the second, and this pins that the first exists only
// for callers that have already checked.
func TestNumeric_ZeroOnError(t *testing.T) {
	nan := []byte{0, 0, 0, 0, 0xC0, 0x00, 0, 0}
	if d := runtime.Numeric(nan); d.Unscaled != 0 {
		t.Errorf("Numeric on NaN = %v, want the zero value", d)
	}
	if _, err := runtime.NumericErr(nan); err == nil {
		t.Error("NumericErr on NaN must report it")
	}
}
