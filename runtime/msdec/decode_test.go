package msdec_test

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/msdec"
)

// The canonical forms runtime/msdrv produces, decoded. The bytes here are
// written by hand rather than captured, because the point is the CONTRACT
// between the two packages: if it changes, one of them should stop compiling
// against these and not silently disagree.

func TestIntegersAreLittleEndian(t *testing.T) {
	if got := msdec.Int1([]byte{0xFF}); got != -1 {
		t.Errorf("Int1 = %d", got)
	}
	if got := msdec.Int2([]byte{0x07, 0x00}); got != 7 {
		t.Errorf("Int2 = %d", got)
	}
	if got := msdec.Int4([]byte{0x46, 0x00, 0x00, 0x00}); got != 70 {
		t.Errorf("Int4 = %d", got)
	}
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], 700)
	if got := msdec.Int8(b8[:]); got != 700 {
		t.Errorf("Int8 = %d", got)
	}
	// A short value is a bug to report, not a panic. Nothing a server that
	// agrees with the schema can send — which is exactly why it must not take
	// the process down when something does.
	if msdec.Int8([]byte{1}) != 0 || msdec.Int4(nil) != 0 || msdec.Int2(nil) != 0 {
		t.Error("a short value did not decode to zero")
	}
	if msdec.Bool([]byte{1}) != true || msdec.Bool([]byte{0}) != false || msdec.Bool(nil) {
		t.Error("Bool")
	}
}

func TestFloats(t *testing.T) {
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], math.Float32bits(1.5))
	if got := msdec.Float4(b4[:]); got != 1.5 {
		t.Errorf("Float4 = %v", got)
	}
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], math.Float64bits(2.25))
	if got := msdec.Float8(b8[:]); got != 2.25 {
		t.Errorf("Float8 = %v", got)
	}
}

// An nvarchar is UTF-16LE, which is the difference that makes this a transcode
// rather than a copy. A scanner that let the slab take the bytes verbatim
// returns the interleaved-null form — not an error, and not the string.
func TestStringsAreUTF16(t *testing.T) {
	var sl runtime.Slab
	ascii := []byte{'h', 0, 'i', 0}
	if got := msdec.Str(ascii, &sl); got != "hi" {
		t.Errorf("Str = %q", got)
	}
	// Beyond ASCII, and beyond the BMP, through the slow path.
	wide := utf16le("héllo 🌩")
	if got := msdec.Str(wide, &sl); got != "héllo 🌩" {
		t.Errorf("Str = %q", got)
	}
	if got := msdec.Str(nil, &sl); got != "" {
		t.Errorf("Str(nil) = %q", got)
	}
	// NULL is nil and an empty string is not.
	if v := msdec.NullText(nil, &sl); v.Valid {
		t.Error("nil decoded as a present value")
	}
	if v := msdec.NullText([]byte{}, &sl); !v.Valid || v.V != "" {
		t.Errorf("an empty string decoded as %+v", v)
	}
}

func utf16le(s string) []byte {
	var out []byte
	for _, r := range s {
		if r > 0xFFFF {
			r -= 0x10000
			hi := 0xD800 + (r >> 10)
			lo := 0xDC00 + (r & 0x3FF)
			out = append(out, byte(hi), byte(hi>>8), byte(lo), byte(lo>>8))
			continue
		}
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

// The temporal forms, which msdrv normalises to scale 7 precisely so this
// package does not need the column metadata to read them.
func TestTemporals(t *testing.T) {
	want := time.Date(2026, 9, 20, 14, 30, 45, 123456700, time.UTC)
	if got, err := msdec.DateTimeOffset(canonicalTime(want, true)); err != nil {
		t.Fatal(err)
	} else if !got.Equal(want) {
		t.Errorf("DateTimeOffset = %v, want %v", got, want)
	}
	if got, err := msdec.DateTime(canonicalTime(want, false)); err != nil {
		t.Fatal(err)
	} else if !got.Equal(want) {
		t.Errorf("DateTime = %v, want %v", got, want)
	}

	day := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	if got, err := msdec.Date(canonicalTime(day, false)[5:8]); err != nil {
		t.Fatal(err)
	} else if !got.Equal(day) {
		t.Errorf("Date = %v, want %v", got, day)
	}

	// TimeOfDay counts MICROSECONDS and the wire counts hundreds of
	// nanoseconds, so the conversion is a division by ten. M9 got the same
	// factor wrong in the other direction and produced a value a thousand
	// times too large.
	ticks := uint64(14*3600+30*60) * 10000000
	var tb [5]byte
	for i := 0; i < 5; i++ {
		tb[i] = byte(ticks >> (8 * i))
	}
	got, err := msdec.TimeOfDay(tb[:])
	if err != nil {
		t.Fatal(err)
	}
	if want := runtime.TimeOfDay((14*3600 + 30*60) * 1000000); got != want {
		t.Errorf("TimeOfDay = %d, want %d", got, want)
	}

	// Short values report rather than return a plausible instant.
	if _, err := msdec.DateTimeOffset([]byte{1, 2}); err == nil {
		t.Error("a short datetimeoffset decoded")
	}
	if _, err := msdec.Date([]byte{1}); err == nil {
		t.Error("a short date decoded")
	}
	if _, err := msdec.TimeOfDay([]byte{1}); err == nil {
		t.Error("a short time decoded")
	}
}

// canonicalTime builds what msdrv hands over: five bytes of hundred-nanosecond
// ticks, three of day number, and optionally two of offset.
func canonicalTime(t time.Time, withOffset bool) []byte {
	u := t.UTC()
	ticks := uint64(u.Hour())*3600 + uint64(u.Minute())*60 + uint64(u.Second())
	ticks = ticks*10000000 + uint64(u.Nanosecond())/100
	n := 8
	if withOffset {
		n = 10
	}
	b := make([]byte, n)
	for i := 0; i < 5; i++ {
		b[i] = byte(ticks >> (8 * i))
	}
	day := uint32(u.Unix()/86400) + 719162
	b[5], b[6], b[7] = byte(day), byte(day>>8), byte(day>>16)
	return b
}

// The nine canonical bytes: scale, then the unscaled int64.
func TestDecimal(t *testing.T) {
	b := make([]byte, 9)
	b[0] = 4
	binary.LittleEndian.PutUint64(b[1:], uint64(int64(1234567)))
	d, err := msdec.Decimal(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Unscaled != 1234567 || d.Scale != 4 {
		t.Errorf("Decimal = %+v", d)
	}
	if got := d.String(); got != "123.4567" {
		t.Errorf("rendered as %q", got)
	}

	// Negative.
	neg := int64(-50)
	binary.LittleEndian.PutUint64(b[1:], uint64(neg))
	if d, err := msdec.Decimal(b); err != nil || d.Unscaled != -50 {
		t.Errorf("negative: %+v %v", d, err)
	}

	// Past eighteen significant digits, which msdrv flags rather than
	// truncating: a currency figure quietly missing its top digits is the worst
	// outcome available.
	b[0] = 0xFF
	if _, err := msdec.Decimal(b); err != runtime.ErrDecimalRange {
		t.Errorf("an overflowing decimal returned %v", err)
	}
	if _, err := msdec.Decimal([]byte{1, 2}); err == nil {
		t.Error("a short decimal decoded")
	}
}

// A uuid arrives already swapped — msdrv does it, because a decoder that did it
// here would have to know which back end it was decoding for.
func TestUUIDIsPassedThrough(t *testing.T) {
	want := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	if got := msdec.UUID(want[:]); got != want {
		t.Errorf("UUID = %v", got)
	}
}

// Every nullable spelling, because missing one is what shipped a call to a
// function that did not exist: no MySQL fixture had a nullable temporal column,
// and a soft-delete model has one on day one.
func TestNullableSpellings(t *testing.T) {
	var sl runtime.Slab
	if v, err := msdec.NullNumeric(nil); err != nil || v.Valid {
		t.Errorf("NullNumeric(nil) = %+v %v", v, err)
	}
	if v, err := msdec.NullDateTimeOffset(nil); err != nil || v.Valid {
		t.Errorf("NullDateTimeOffset(nil) = %+v %v", v, err)
	}
	if v, err := msdec.NullDateTime(nil); err != nil || v.Valid {
		t.Errorf("NullDateTime(nil) = %+v %v", v, err)
	}
	if v, err := msdec.NullDate(nil); err != nil || v.Valid {
		t.Errorf("NullDate(nil) = %+v %v", v, err)
	}
	if v, err := msdec.NullTimeOfDay(nil); err != nil || v.Valid {
		t.Errorf("NullTimeOfDay(nil) = %+v %v", v, err)
	}
	if v := msdec.NullJSON(nil, &sl); v.Valid {
		t.Error("NullJSON(nil) is present")
	}

	// And each carries the value through when there is one.
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if v, err := msdec.NullDateTimeOffset(canonicalTime(now, true)); err != nil || !v.Valid ||
		!v.V.Equal(now) {
		t.Errorf("NullDateTimeOffset = %+v %v", v, err)
	}
	b := make([]byte, 9)
	b[0] = 2
	binary.LittleEndian.PutUint64(b[1:], 150)
	if v, err := msdec.NullNumeric(b); err != nil || !v.Valid || v.V.Unscaled != 150 {
		t.Errorf("NullNumeric = %+v %v", v, err)
	}
	// A short value propagates its error rather than a zero value.
	if _, err := msdec.NullNumeric([]byte{1}); err == nil {
		t.Error("a short nullable decimal decoded")
	}
	if _, err := msdec.NullDate([]byte{1}); err == nil {
		t.Error("a short nullable date decoded")
	}
	if _, err := msdec.NullTimeOfDay([]byte{1}); err == nil {
		t.Error("a short nullable time decoded")
	}
	if _, err := msdec.NullDateTime([]byte{1}); err == nil {
		t.Error("a short nullable datetime decoded")
	}
}

func TestBytesAndJSON(t *testing.T) {
	var sl runtime.Slab
	src := []byte{0xde, 0xad, 0xbe, 0xef}
	got := msdec.Bytes(src, &sl)
	if len(got) != 4 || got[0] != 0xde || got[3] != 0xef {
		t.Errorf("Bytes = % x", got)
	}
	// Copied into the slab, so mutating the source cannot reach it.
	src[0] = 0
	if got[0] != 0xde {
		t.Error("Bytes aliased the wire buffer")
	}
	if msdec.Bytes(nil, &sl) != nil {
		t.Error("Bytes(nil) is not nil")
	}

	doc := utf16le(`{"a":1}`)
	if string(msdec.JSONB(doc, &sl)) != `{"a":1}` {
		t.Errorf("JSONB = %s", msdec.JSONB(doc, &sl))
	}
	if msdec.JSONB(nil, &sl) != nil {
		t.Error("JSONB(nil) is not nil")
	}
	if string(msdec.JSON([]byte(`{}`))) != `{}` {
		t.Error("JSON")
	}
}
