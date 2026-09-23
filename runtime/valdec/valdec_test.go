package valdec_test

// Every mapping here was MEASURED against go-ora and Oracle Free 23 —
// internal/oraclespike/valuetypes_test.go, 2026-09-23. These tests pin the
// results that decided the design, so a "simplification" that drops the string
// cases fails rather than silently rounding somebody's money.

import (
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/valdec"
)

// THE result. Every Oracle NUMBER arrives as an exact decimal string, and that
// is what makes a value path lossless: a float64 cannot hold 2^53+1.
func TestAnIntegerPast2To53SurvivesAsText(t *testing.T) {
	const big = int64(9007199254740993) // 2^53 + 1
	if got := valdec.Int8("9007199254740993"); got != big {
		t.Errorf("Int8(string) = %d, want %d", got, big)
	}
	// And what would have happened through a float64, so the reason is in the
	// test rather than only in a comment.
	if int64(float64(big)) == big {
		t.Skip("this platform's float64 holds 2^53+1, which it should not")
	}
	if got := valdec.Int8(float64(big)); got == big {
		t.Error("a float64 round trip preserved 2^53+1, so this test proves nothing")
	}
}

// A money column through a float64 is a money column with a rounding error, so
// the float case is REFUSED rather than accepted — the loss happened before
// storm was asked and the error is the only place to say so.
func TestAnExactNumericRefusesAFloat(t *testing.T) {
	d, err := valdec.Decimal("12345.6789")
	if err != nil {
		t.Fatalf("a decimal string must decode: %v", err)
	}
	if d.String() != "12345.6789" {
		t.Errorf("got %s, want 12345.6789", d.String())
	}
	if _, err := valdec.Decimal(12345.6789); err == nil {
		t.Error("a float64 numeric must be refused: the precision is already gone")
	}
}

// go-ora reports a 23c native BOOLEAN as database type NUMBER and hands back
// "1". A decoder that only handled the bool case would read every true as
// false, silently, for every row.
func TestBooleanArrivesAsText(t *testing.T) {
	for _, v := range []any{"1", true, int64(1), "true", "Y"} {
		if !valdec.Bool(v) {
			t.Errorf("Bool(%#v) = false", v)
		}
	}
	for _, v := range []any{"0", false, int64(0), nil} {
		if valdec.Bool(v) {
			t.Errorf("Bool(%#v) = true", v)
		}
	}
}

// NULL stays distinct from zero, which is what the whole Oracle milestone is
// about one layer up.
func TestNullIsNotZero(t *testing.T) {
	if !valdec.IsNull(nil) {
		t.Error("nil is SQL NULL")
	}
	for _, v := range []any{int64(0), "", false, []byte(nil)} {
		if valdec.IsNull(v) {
			t.Errorf("%#v is a value, not NULL", v)
		}
	}
}

// A driver may reuse its buffer on the next Next, so a []byte column is copied
// — the hazard the byte families answer with a slab.
func TestBytesAreCopied(t *testing.T) {
	buf := []byte{1, 2, 3}
	got := valdec.Bytes(buf)
	buf[0] = 9
	if got[0] != 1 {
		t.Error("the decoded slice aliases the driver's buffer")
	}
}

func TestTimeKeepsItsOffset(t *testing.T) {
	want := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.FixedZone("", 2*3600))
	got, err := valdec.Time(want)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) || got.Format(time.RFC3339) != want.Format(time.RFC3339) {
		t.Errorf("got %s, want %s", got, want)
	}
	// And the text form, for a driver that hands temporals back as strings.
	if _, err := valdec.Time("2026-01-02 03:04:05.123456 +02:00"); err != nil {
		t.Errorf("a text temporal must parse: %v", err)
	}
	if _, err := valdec.Time(42); err == nil {
		t.Error("an int is not a time and must not become one")
	}
}

// The canonical names, which this family provides unrenamed — the rename map
// is empty, and a generated package calls valdec.Timestamptz by the same name
// runtime's own family uses.
func TestTheCanonicalNamesDecode(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	if got := valdec.UUID([]byte{1, 2, 3}); got[0] != 1 || got[2] != 3 || got[15] != 0 {
		t.Errorf("UUID = %v", got)
	}
	// A RAW(16) arrives as []byte and is COPIED into the array — which is also
	// why codegen cannot emit the byte families' copy() for this shape.
	if got := valdec.UUID(nil); got != [16]byte{} {
		t.Errorf("a NULL uuid must be the zero key, got %v", got)
	}

	if got, err := valdec.Timestamptz(ts); err != nil || !got.Equal(ts) {
		t.Errorf("Timestamptz = %v, %v", got, err)
	}
	if got, err := valdec.Date(ts); err != nil || !got.Equal(ts) {
		t.Errorf("Date = %v, %v", got, err)
	}
	if _, err := valdec.TimeOfDay(ts); err != nil {
		t.Errorf("TimeOfDay: %v", err)
	}
	if got, err := valdec.NumericErr("1.25"); err != nil || got.String() != "1.25" {
		t.Errorf("NumericErr = %v, %v", got, err)
	}
}

// The Null wrappers pair the NULL test with the error, which is the convention
// every family follows for a decoder that can fail.
func TestTheNullWrappersKeepAbsentDistinct(t *testing.T) {
	if n, err := valdec.NullNumeric(nil); err != nil || n.Valid {
		t.Errorf("NullNumeric(nil) = %+v, %v", n, err)
	}
	if n, err := valdec.NullNumeric("2.5"); err != nil || !n.Valid || n.V.String() != "2.5" {
		t.Errorf("NullNumeric = %+v, %v", n, err)
	}
	if n, err := valdec.NullTimestamptz(nil); err != nil || n.Valid {
		t.Errorf("NullTimestamptz(nil) = %+v, %v", n, err)
	}
	ts := time.Now().UTC()
	if n, err := valdec.NullTimestamptz(ts); err != nil || !n.Valid {
		t.Errorf("NullTimestamptz = %+v, %v", n, err)
	}
	if n, err := valdec.NullDate(nil); err != nil || n.Valid {
		t.Errorf("NullDate(nil) = %+v, %v", n, err)
	}
	if n, err := valdec.NullTimeOfDay(nil); err != nil || n.Valid {
		t.Errorf("NullTimeOfDay(nil) = %+v, %v", n, err)
	}
	if n, err := valdec.NullTimeOfDay(ts); err != nil || !n.Valid {
		t.Errorf("NullTimeOfDay = %+v, %v", n, err)
	}

	// NullText takes a slab it does not use: the driver already allocated the
	// string, so copying it in would be a second allocation for no lifetime
	// benefit.
	var sl runtime.Slab
	if n := valdec.NullText(nil, &sl); n.Valid {
		t.Error("NullText(nil) must be invalid")
	}
	if n := valdec.NullText("x", &sl); !n.Valid || n.V != "x" {
		t.Errorf("NullText = %+v", n)
	}

	if got := valdec.JSONB([]byte(`{"a":1}`), &sl); string(got) != `{"a":1}` {
		t.Errorf("JSONB = %s", got)
	}
	if n := valdec.NullJSONB(nil, &sl); n.Valid {
		t.Error("NullJSONB(nil) must be invalid")
	}
	if n := valdec.NullJSONB([]byte("{}"), &sl); !n.Valid {
		t.Error("NullJSONB must carry a document")
	}

	// Nullable wraps a decoder that cannot fail.
	if n := valdec.Nullable(nil, valdec.Int8); n.Valid {
		t.Error("Nullable(nil) must be invalid")
	}
	if n := valdec.Nullable(any("7"), valdec.Int8); !n.Valid || n.V != 7 {
		t.Errorf("Nullable = %+v", n)
	}
}

// The narrow widths, and the floats.
func TestTheNarrowDecoders(t *testing.T) {
	if got := valdec.Int4("2000000000"); got != 2000000000 {
		t.Errorf("Int4 = %d", got)
	}
	if got := valdec.Int2("32000"); got != 32000 {
		t.Errorf("Int2 = %d", got)
	}
	// go-ora yields BINARY_FLOAT as a float32 and BINARY_DOUBLE as a float64.
	if got := valdec.Float4(float32(1.5)); got != 1.5 {
		t.Errorf("Float4 = %v", got)
	}
	if got := valdec.Float8(float64(1.25)); got != 1.25 {
		t.Errorf("Float8 = %v", got)
	}
	// And the text forms, because a NUMBER arrives as one.
	if got := valdec.Float8("1.25"); got != 1.25 {
		t.Errorf("Float8(string) = %v", got)
	}
	for _, v := range []any{nil, struct{}{}} {
		if valdec.Int8(v) != 0 || valdec.Float8(v) != 0 {
			t.Errorf("an unreadable %T must be zero, not a guess", v)
		}
	}
	if got := valdec.Str(42); got != "42" {
		t.Errorf("Str of a non-string = %q", got)
	}
	if valdec.Bytes(nil) != nil {
		t.Error("Bytes(nil) must be nil, not empty")
	}
	if got := valdec.Bytes("ab"); string(got) != "ab" {
		t.Errorf("Bytes(string) = %s", got)
	}
}
