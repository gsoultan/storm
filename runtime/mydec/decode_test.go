package mydec_test

import (
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/mydec"
)

// The whole reason this package exists. MySQL is little-endian where
// PostgreSQL is big-endian, so the SAME bytes are two different numbers — and
// reading one with the other's decoder returns a byte-reversed value without
// erroring, for every row. These assert the two disagree, so nobody can
// "simplify" by pointing codegen at the wrong family.
func TestEndiannessDiffersFromPostgres(t *testing.T) {
	// 1 as a MySQL BIGINT.
	b := []byte{1, 0, 0, 0, 0, 0, 0, 0}
	if got := mydec.Int8(b); got != 1 {
		t.Errorf("mydec.Int8 = %d, want 1", got)
	}
	if got := runtime.Int8(b); got == 1 {
		t.Fatal("the PostgreSQL decoder read a MySQL BIGINT correctly; " +
			"if that were true this package would not need to exist")
	}
	if got := runtime.Int8(b); got != 72057594037927936 {
		t.Errorf("sanity: big-endian read of the same bytes = %d", got)
	}
}

func TestIntegers(t *testing.T) {
	if got := mydec.Int2([]byte{0x2a, 0x00}); got != 42 {
		t.Errorf("Int2 = %d", got)
	}
	if got := mydec.Int4([]byte{0xd2, 0x04, 0x00, 0x00}); got != 1234 {
		t.Errorf("Int4 = %d", got)
	}
	// Negative: two's complement, little-endian.
	if got := mydec.Int4([]byte{0xff, 0xff, 0xff, 0xff}); got != -1 {
		t.Errorf("Int4(-1) = %d", got)
	}
	if got := mydec.Int1([]byte{0xff}); got != -1 {
		t.Errorf("Int1(-1) = %d", got)
	}
}

// A short value must not panic. A truncated packet is a wire fault, and
// panicking in a scanner takes the process with it.
func TestShortValuesDoNotPanic(t *testing.T) {
	for _, b := range [][]byte{nil, {}, {1}, {1, 2, 3}} {
		mydec.Int2(b)
		mydec.Int4(b)
		mydec.Int8(b)
		mydec.Float4(b)
		mydec.Float8(b)
		mydec.UUID(b)
	}
}

func TestFloats(t *testing.T) {
	// 1.5 as IEEE754 little-endian.
	if got := mydec.Float8([]byte{0, 0, 0, 0, 0, 0, 0xf8, 0x3f}); got != 1.5 {
		t.Errorf("Float8 = %v", got)
	}
	if got := mydec.Float4([]byte{0, 0, 0xc0, 0x3f}); got != 1.5 {
		t.Errorf("Float4 = %v", got)
	}
}

// MySQL packs a DATETIME component-wise with a LEADING LENGTH, and every
// shorter form is legal. A decoder that assumed the widest form would read past
// the value on the common case.
func TestDateTimeLengths(t *testing.T) {
	// 11 bytes: full precision. 2026-06-01 12:34:56.789012
	full := []byte{11, 0xea, 0x07, 6, 1, 12, 34, 56, 0x14, 0x0a, 0x0c, 0x00}
	got, err := mydec.DateTime(full)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 1, 12, 34, 56, 789012000, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// 7 bytes: no microseconds.
	sec := []byte{7, 0xea, 0x07, 6, 1, 12, 34, 56}
	got, err = mydec.DateTime(sec)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(time.Date(2026, 6, 1, 12, 34, 56, 0, time.UTC)) {
		t.Errorf("7-byte form: %v", got)
	}

	// 4 bytes: a bare date.
	day := []byte{4, 0xea, 0x07, 6, 1}
	got, err = mydec.DateTime(day)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("4-byte form: %v", got)
	}

	// 0 bytes: the zero date.
	if got, err := mydec.DateTime([]byte{0}); err != nil || !got.IsZero() {
		t.Errorf("zero form: %v %v", got, err)
	}
}

// UTC, not Local: MySQL's DATETIME carries no zone, storm writes UTC, so
// reading it back as UTC is the round trip. time.Local would make the value
// depend on where the process runs.
func TestDateTimeIsUTC(t *testing.T) {
	got, err := mydec.DateTime([]byte{7, 0xea, 0x07, 6, 1, 12, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if got.Location() != time.UTC {
		t.Errorf("decoded in %v, want UTC", got.Location())
	}
}

func TestDateTimeRejectsBadLengths(t *testing.T) {
	for _, b := range [][]byte{{5, 1, 2, 3, 4, 5}, {11, 1}, {8, 1, 2, 3, 4, 5, 6, 7, 8}} {
		if _, err := mydec.DateTime(b); err == nil {
			t.Errorf("%v was accepted", b)
		}
	}
}

// MySQL's TIME is a signed DURATION, not a time of day — it ranges beyond ±24h
// on purpose, which is why it decodes to time.Duration.
func TestDurationIsSignedAndCanExceedADay(t *testing.T) {
	// -2 days 03:04:05.000006
	b := []byte{12, 1, 2, 0, 0, 0, 3, 4, 5, 6, 0, 0, 0}
	got, err := mydec.Duration(b)
	if err != nil {
		t.Fatal(err)
	}
	want := -(48*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second + 6*time.Microsecond)
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

// MySQL sends DECIMAL as TEXT even in the binary protocol.
func TestDecimalIsParsedFromText(t *testing.T) {
	for _, c := range []struct {
		in    string
		uns   int64
		scale int32
	}{
		{"0", 0, 0},
		{"42", 42, 0},
		{"19.99", 1999, 2},
		{"-0.10", -10, 2},
		{"+3.5", 35, 1},
	} {
		d, err := mydec.Decimal([]byte(c.in))
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if d.Unscaled != c.uns || d.Scale != c.scale {
			t.Errorf("%s → %d/%d, want %d/%d", c.in, d.Unscaled, d.Scale, c.uns, c.scale)
		}
	}
}

// Too many digits is an error, not a wrap. Same ceiling and same reasoning as
// the PostgreSQL family.
func TestDecimalRefusesOverflow(t *testing.T) {
	if _, err := mydec.Decimal([]byte("1234567890123456789")); err == nil {
		t.Error("19 significant digits were accepted")
	}
	if _, err := mydec.Decimal([]byte("12.x")); err == nil {
		t.Error("a non-digit was accepted")
	}
}

// A uuid is an opaque identifier, not a number: reversing it would produce a
// different, valid-looking uuid — the quietest possible corruption.
func TestUUIDIsNotByteReversed(t *testing.T) {
	in := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	got := mydec.UUID(in)
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("byte %d is %d, want %d — the uuid was reordered", i, got[i], in[i])
		}
	}
}

// The budget: no allocation beyond the string copy a Go string requires.
func TestDecodersDoNotAllocate(t *testing.T) {
	i8 := []byte{1, 0, 0, 0, 0, 0, 0, 0}
	dt := []byte{11, 0xea, 0x07, 6, 1, 12, 34, 56, 0x14, 0x0a, 0x0c, 0x00}
	dec := []byte("19.99")
	if got := testing.AllocsPerRun(200, func() {
		_ = mydec.Int8(i8)
		_ = mydec.Float8(i8)
		_ = mydec.UUID(dt)
		_, _ = mydec.DateTime(dt)
		_, _ = mydec.Decimal(dec)
	}); got != 0 {
		t.Errorf("decoders allocate %.0f time(s) per row; the budget is 0", got)
	}
}

// A TIME is MICROSECONDS once it is a runtime.TimeOfDay, and nanoseconds while
// it is a time.Duration. A plain cast between them is a value a thousand times
// too large — and one that stores and renders without complaint, which is the
// kind of wrong answer this package exists to prevent.
func TestTimeOfDayIsMicroseconds(t *testing.T) {
	// 01:02:03.500000, packed the way MySQL sends it.
	b := []byte{12, 0, 0, 0, 0, 0, 1, 2, 3, 0x20, 0xa1, 0x07, 0x00}
	got, err := mydec.TimeOfDay(b)
	if err != nil {
		t.Fatal(err)
	}
	want := runtime.TimeOfDay((1*time.Hour + 2*time.Minute + 3*time.Second +
		500*time.Millisecond) / time.Microsecond)
	if got != want {
		t.Errorf("TimeOfDay = %d, want %d", int64(got), int64(want))
	}
	if h, m, s, us := got.Parts(); h != 1 || m != 2 || s != 3 || us != 500000 {
		t.Errorf("parts = %d:%d:%d.%06d, want 01:02:03.500000", h, m, s, us)
	}
}

func TestNullTimeOfDay(t *testing.T) {
	got, err := mydec.NullTimeOfDay(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Error("a NULL came back valid")
	}
	b := []byte{8, 0, 0, 0, 0, 0, 1, 2, 3}
	got, err = mydec.NullTimeOfDay(b)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.V != runtime.TimeOfDay((1*time.Hour+2*time.Minute+3*time.Second)/time.Microsecond) {
		t.Errorf("NullTimeOfDay = %+v", got)
	}
}

// MySQL sends a JSON column as TEXT — its internal binary form never reaches a
// client — so unlike PostgreSQL there is no version byte to strip. Stripping
// one would eat the opening brace.
func TestJSONKeepsItsFirstByte(t *testing.T) {
	var sl runtime.Slab
	doc := []byte(`{"a":1}`)
	if got := mydec.JSONB(doc, &sl); string(got) != `{"a":1}` {
		t.Errorf("JSONB = %s, want the whole document", got)
	}
	if got := mydec.JSON([]byte(`[1]`)); string(got) != `[1]` {
		t.Errorf("JSON = %s", got)
	}
	if got := mydec.JSONB(nil, &sl); got != nil {
		t.Errorf("an empty document decoded to %v, want nil", got)
	}
}

// The scalar and nullable decoders that no test reached.
//
// They are one-liners, which is exactly why they were missed and exactly why
// they are worth pinning: a decoder that returns a plausible value for the
// wrong bytes produces wrong ROWS, not an error, and this family already shipped
// one of those — the nullable temporals were absent for months because no
// fixture had a nullable timestamp column and the generated package named a
// function from the PostgreSQL family instead.

func TestBoolReadsTheFirstByte(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want bool
	}{
		{"zero", []byte{0}, false},
		{"one", []byte{1}, true},
		{"any non-zero", []byte{0x7f}, true},
		{"empty is false, not a panic", nil, false},
		// MySQL sends BOOLEAN as TINYINT, so -1 is true like any other
		// non-zero. A decoder testing `== 1` would call this false.
		{"minus one", []byte{0xff}, true},
	} {
		if got := mydec.Bool(tc.in); got != tc.want {
			t.Errorf("%s: Bool(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestInt1IsSigned(t *testing.T) {
	// The bug this catches: reading a TINYINT as a byte makes -1 into 255.
	if got := mydec.Int1([]byte{0xff}); got != -1 {
		t.Errorf("Int1(0xff) = %d, want -1", got)
	}
	if got := mydec.Int1([]byte{0x80}); got != -128 {
		t.Errorf("Int1(0x80) = %d, want -128", got)
	}
	if got := mydec.Int1(nil); got != 0 {
		t.Errorf("Int1(nil) = %d, want 0", got)
	}
}

// Text and Bytes must COPY: the wire buffer is reused on the next row, so a
// decoder that aliased it would return the following row's bytes from a value
// the caller already holds.
func TestTextAndBytesCopyTheWireBuffer(t *testing.T) {
	buf := []byte("hello")
	s := mydec.Text(buf)
	b := mydec.Bytes(buf)

	copy(buf, "world") // the driver reusing the buffer for the next row

	if s != "hello" {
		t.Errorf("Text aliased the wire buffer: %q", s)
	}
	if string(b) != "hello" {
		t.Errorf("Bytes aliased the wire buffer: %q", b)
	}
	if mydec.Bytes(nil) != nil {
		t.Error("Bytes(nil) should stay nil, not become an empty slice")
	}
	if mydec.Text(nil) != "" {
		t.Error(`Text(nil) should be ""`)
	}
}

func TestDateReadsMidnight(t *testing.T) {
	// MySQL sends a DATE as the same structure as a DATETIME, truncated: a
	// length byte then year(2), month, day.
	got, err := mydec.Date([]byte{4, 0xe9, 0x07, 3, 14})
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2025, 3, 14, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Date = %v, want %v", got, want)
	}
	// A zero-length DATE is MySQL's all-zero date, not an error.
	if _, err := mydec.Date(nil); err != nil {
		t.Errorf("Date(nil) = %v, want no error", err)
	}
}

// Every nullable decoder has the same two jobs: nil is NULL and not an error,
// and a present value decodes to exactly what the non-null decoder returns.
// Tested together because a family where one of them forgets is the failure
// that actually happened here.
func TestNullDecodersTreatNilAsNull(t *testing.T) {
	var s runtime.Slab

	if v := mydec.NullText(nil, &s); v.Valid {
		t.Error("NullText(nil) is valid")
	}
	if v := mydec.NullText([]byte("hi"), &s); !v.Valid || v.V != "hi" {
		t.Errorf("NullText = %+v, want hi", v)
	}

	if v := mydec.NullJSON(nil, &s); v.Valid {
		t.Error("NullJSON(nil) is valid")
	}
	if v := mydec.NullJSON([]byte(`{"a":1}`), &s); !v.Valid || string(v.V) != `{"a":1}` {
		t.Errorf("NullJSON = %+v", v)
	}

	nn, err := mydec.NullNumeric(nil)
	if err != nil || nn.Valid {
		t.Errorf("NullNumeric(nil) = %+v, %v", nn, err)
	}
	nn, err = mydec.NullNumeric([]byte("12.34"))
	if err != nil || !nn.Valid {
		t.Fatalf("NullNumeric = %+v, %v", nn, err)
	}
	if nn.V.String() != "12.34" {
		t.Errorf("NullNumeric value = %s, want 12.34", nn.V.String())
	}

	nd, err := mydec.NullDateTime(nil)
	if err != nil || nd.Valid {
		t.Errorf("NullDateTime(nil) = %+v, %v", nd, err)
	}
	nd, err = mydec.NullDateTime([]byte{4, 0xe9, 0x07, 3, 14})
	if err != nil || !nd.Valid {
		t.Fatalf("NullDateTime = %+v, %v", nd, err)
	}
	if want := time.Date(2025, 3, 14, 0, 0, 0, 0, time.UTC); !nd.V.Equal(want) {
		t.Errorf("NullDateTime value = %v, want %v", nd.V, want)
	}

	nda, err := mydec.NullDate(nil)
	if err != nil || nda.Valid {
		t.Errorf("NullDate(nil) = %+v, %v", nda, err)
	}
	nda, err = mydec.NullDate([]byte{4, 0xe9, 0x07, 3, 14})
	if err != nil || !nda.Valid {
		t.Fatalf("NullDate = %+v, %v", nda, err)
	}

	ndu, err := mydec.NullDuration(nil)
	if err != nil || ndu.Valid {
		t.Errorf("NullDuration(nil) = %+v, %v", ndu, err)
	}
	// 8 bytes: negative flag, days(4), h, m, s — MySQL's TIME goes past 24h
	// and past zero, which is why it decodes to a Duration and not a clock.
	ndu, err = mydec.NullDuration([]byte{8, 1, 0, 0, 0, 0, 1, 2, 3})
	if err != nil || !ndu.Valid {
		t.Fatalf("NullDuration = %+v, %v", ndu, err)
	}
	if ndu.V >= 0 {
		t.Errorf("NullDuration = %v, want a negative duration", ndu.V)
	}
}

// A malformed value must be an ERROR, not a plausible decode — the nullable
// wrappers must not swallow what the inner decoder reports.
func TestNullDecodersPropagateAFormatError(t *testing.T) {
	if _, err := mydec.NullDuration([]byte{8, 1}); err == nil {
		t.Error("NullDuration accepted a truncated TIME")
	}
	if _, err := mydec.NullDateTime([]byte{11, 0xe9}); err == nil {
		t.Error("NullDateTime accepted a truncated DATETIME")
	}
}
