package valdec_test

// Every mapping here was MEASURED against go-ora and Oracle Free 23 —
// internal/oraclespike/valuetypes_test.go, 2026-09-23. These tests pin the
// results that decided the design, so a "simplification" that drops the string
// cases fails rather than silently rounding somebody's money.

import (
	"testing"
	"time"

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
