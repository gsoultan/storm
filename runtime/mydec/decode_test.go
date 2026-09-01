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
		uns, scale, err := mydec.Decimal([]byte(c.in))
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if uns != c.uns || scale != c.scale {
			t.Errorf("%s → %d/%d, want %d/%d", c.in, uns, scale, c.uns, c.scale)
		}
	}
}

// Too many digits is an error, not a wrap. Same ceiling and same reasoning as
// the PostgreSQL family.
func TestDecimalRefusesOverflow(t *testing.T) {
	if _, _, err := mydec.Decimal([]byte("1234567890123456789")); err == nil {
		t.Error("19 significant digits were accepted")
	}
	if _, _, err := mydec.Decimal([]byte("12.x")); err == nil {
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
		_, _, _ = mydec.Decimal(dec)
	}); got != 0 {
		t.Errorf("decoders allocate %.0f time(s) per row; the budget is 0", got)
	}
}
