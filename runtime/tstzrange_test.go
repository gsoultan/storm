package runtime_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
)

func tt(h int) time.Time { return time.Date(2026, 6, 1, h, 0, 0, 0, time.UTC) }

// Encode and decode are one round trip or they are a corruption. The bounds
// are the part that matters: a range that comes back inclusive when it was
// declared half-open makes two adjacent bookings collide.
func TestTstzRangeRoundTrip(t *testing.T) {
	for _, c := range []struct {
		name string
		in   runtime.TstzRange
	}{
		{"half-open", runtime.NewTstzRange(tt(9), tt(11))},
		{"closed", runtime.TstzRange{Lower: tt(9), Upper: tt(11), LowerInc: true, UpperInc: true}},
		{"open", runtime.TstzRange{Lower: tt(9), Upper: tt(11)}},
		{"since", runtime.Since(tt(9))},
		{"until", runtime.Until(tt(11))},
		{"empty", runtime.TstzRange{Empty: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := runtime.DecodeTstzRange(runtime.EncodeTstzRange(c.in, nil))
			if err != nil {
				t.Fatal(err)
			}
			if got.Empty != c.in.Empty {
				t.Fatalf("Empty: %v, want %v", got.Empty, c.in.Empty)
			}
			if c.in.Empty {
				return
			}
			if got.LowerInc != c.in.LowerInc || got.UpperInc != c.in.UpperInc {
				t.Errorf("bounds %v/%v, want %v/%v",
					got.LowerInc, got.UpperInc, c.in.LowerInc, c.in.UpperInc)
			}
			if got.LowerInf != c.in.LowerInf || got.UpperInf != c.in.UpperInf {
				t.Errorf("infinities %v/%v, want %v/%v",
					got.LowerInf, got.UpperInf, c.in.LowerInf, c.in.UpperInf)
			}
			if !c.in.LowerInf && !got.Lower.Equal(c.in.Lower) {
				t.Errorf("lower %v, want %v", got.Lower, c.in.Lower)
			}
			if !c.in.UpperInf && !got.Upper.Equal(c.in.Upper) {
				t.Errorf("upper %v, want %v", got.Upper, c.in.Upper)
			}
		})
	}
}

func TestTstzRangeContains(t *testing.T) {
	half := runtime.NewTstzRange(tt(9), tt(11))
	for _, c := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"inside", tt(10), true},
		{"lower bound is inclusive", tt(9), true},
		{"upper bound is exclusive", tt(11), false},
		{"before", tt(8), false},
		{"after", tt(12), false},
	} {
		if got := half.Contains(c.at); got != c.want {
			t.Errorf("%s: Contains(%v) = %v, want %v", c.name, c.at, got, c.want)
		}
	}
	if runtime.Since(tt(9)).Contains(tt(99)) != true {
		t.Error("an unbounded upper does not contain a far future instant")
	}
	if runtime.Until(tt(9)).Contains(tt(1)) != true {
		t.Error("an unbounded lower does not contain a far past instant")
	}
	if (runtime.TstzRange{Empty: true}).Contains(tt(10)) {
		t.Error("the empty range contains something")
	}
}

func TestTstzRangeMalformed(t *testing.T) {
	for _, b := range [][]byte{
		nil,
		{},
		{0x02},                         // says lower-inclusive, carries no bound
		{0x02, 0, 0, 0, 8},             // length but no value
		{0x02, 0, 0, 0, 4, 1, 2, 3, 4}, // wrong bound width
	} {
		if _, err := runtime.DecodeTstzRange(b); err == nil && len(b) > 0 {
			t.Errorf("%v was accepted", b)
		}
	}
	if _, err := runtime.DecodeTstzRange(nil); !errors.Is(err, runtime.ErrRangeFormat) {
		t.Errorf("empty input: %v", err)
	}
}

func TestNullTstzRange(t *testing.T) {
	n, err := runtime.NullTstzRange(nil)
	if err != nil || n.Valid {
		t.Errorf("nil should decode to an invalid Null: %v %v", n, err)
	}
	n, err = runtime.NullTstzRange(runtime.EncodeTstzRange(runtime.NewTstzRange(tt(1), tt(2)), nil))
	if err != nil || !n.Valid {
		t.Fatalf("%v %v", n, err)
	}
	if !n.V.Lower.Equal(tt(1)) {
		t.Errorf("lower %v", n.V.Lower)
	}
	if _, err := runtime.NullTstzRange([]byte{0x02}); err == nil {
		t.Error("a malformed non-nil value was accepted")
	}
}

func TestIsEmpty(t *testing.T) {
	if !(runtime.TstzRange{Empty: true}).IsEmpty() {
		t.Error("IsEmpty is false for the empty range")
	}
	if runtime.NewTstzRange(tt(1), tt(2)).IsEmpty() {
		t.Error("IsEmpty is true for a real range")
	}
}

// The typed constraint errors, matched the way a handler matches them.
func TestConstraintErrorMatching(t *testing.T) {
	inner := errors.New("ERROR: duplicate key (SQLSTATE 23505)")
	ce := &runtime.ConstraintError{
		Kind: runtime.ErrUniqueViolation, Constraint: "uq_x", Table: "t", Column: "c", Err: inner,
	}
	if !errors.Is(ce, runtime.ErrUniqueViolation) {
		t.Error("errors.Is does not match the sentinel")
	}
	if errors.Is(ce, runtime.ErrCheckViolation) {
		t.Error("it matched the wrong sentinel")
	}
	if !errors.Is(ce, inner) {
		t.Error("the driver's error is not reachable")
	}
	if got := ce.Error(); got != "storm: unique constraint violated (uq_x)" {
		t.Errorf("Error() = %q", got)
	}
	// With no constraint name it falls back to the table.
	ce2 := &runtime.ConstraintError{Kind: runtime.ErrCheckViolation, Table: "t"}
	if got := ce2.Error(); got != "storm: check constraint violated (t)" {
		t.Errorf("Error() = %q", got)
	}
	// With neither, just the kind.
	ce3 := &runtime.ConstraintError{Kind: runtime.ErrDeadlock}
	if got := ce3.Error(); got != runtime.ErrDeadlock.Error() {
		t.Errorf("Error() = %q", got)
	}
}

// Overlaps in the runtime package's own tests, not only through the example.
// These are the boundary cases hand-rolled timestamp comparisons get wrong,
// and the Go answer must match what PostgreSQL's && would say.
func TestOverlapsBoundaries(t *testing.T) {
	half := runtime.NewTstzRange(tt(9), tt(11))
	for _, c := range []struct {
		name string
		o    runtime.TstzRange
		want bool
	}{
		{"identical", runtime.NewTstzRange(tt(9), tt(11)), true},
		{"contained", runtime.NewTstzRange(tt(9), tt(10)), true},
		{"straddles the end", runtime.NewTstzRange(tt(10), tt(12)), true},
		{"straddles the start", runtime.NewTstzRange(tt(8), tt(10)), true},
		{"abuts after", runtime.NewTstzRange(tt(11), tt(12)), false},
		{"abuts before", runtime.NewTstzRange(tt(8), tt(9)), false},
		{"disjoint after", runtime.NewTstzRange(tt(13), tt(14)), false},
		{"disjoint before", runtime.NewTstzRange(tt(1), tt(2)), false},
		{"empty", runtime.TstzRange{Empty: true}, false},
		{"unbounded lower", runtime.Until(tt(10)), true},
		{"unbounded lower, abutting", runtime.Until(tt(9)), false},
		{"unbounded upper", runtime.Since(tt(10)), true},
		{"unbounded upper, abutting", runtime.Since(tt(11)), false},
	} {
		if got := half.Overlaps(c.o); got != c.want {
			t.Errorf("%s: [9,11).Overlaps(%v) = %v, want %v", c.name, c.o, got, c.want)
		}
		// Overlap is symmetric; an asymmetric answer is a bug in `before`.
		if got := c.o.Overlaps(half); got != c.want {
			t.Errorf("%s: reversed = %v, want %v", c.name, got, c.want)
		}
	}
	// Two CLOSED ranges that share only their touching instant do overlap.
	a := runtime.TstzRange{Lower: tt(9), Upper: tt(11), LowerInc: true, UpperInc: true}
	b := runtime.TstzRange{Lower: tt(11), Upper: tt(12), LowerInc: true, UpperInc: true}
	if !a.Overlaps(b) {
		t.Error("[9,11] and [11,12] share 11:00 and must overlap")
	}
	// An empty range overlaps nothing, including itself.
	e := runtime.TstzRange{Empty: true}
	if e.Overlaps(e) {
		t.Error("the empty range overlaps itself")
	}
}

func TestTstzRangeErrDecodes(t *testing.T) {
	in := runtime.NewTstzRange(tt(3), tt(4))
	got, err := runtime.TstzRangeErr(runtime.EncodeTstzRange(in, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Lower.Equal(tt(3)) || !got.Upper.Equal(tt(4)) {
		t.Errorf("got %+v", got)
	}
	if _, err := runtime.TstzRangeErr([]byte{0x02}); err == nil {
		t.Error("a malformed value was accepted")
	}
}

// Retryable separates "the client's problem" from "nobody's problem, run it
// again" — a 409 for a duplicate versus a retry for a serialization failure.
func TestRetryableSeparatesTransientFromClientError(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{runtime.ErrSerializationFailure, true},
		{runtime.ErrDeadlock, true},
		{&runtime.ConstraintError{Kind: runtime.ErrDeadlock}, true},
		{&runtime.ConstraintError{Kind: runtime.ErrSerializationFailure}, true},
		{runtime.ErrUniqueViolation, false},
		{&runtime.ConstraintError{Kind: runtime.ErrUniqueViolation}, false},
		{runtime.ErrCheckViolation, false},
		{errors.New("something else"), false},
		{nil, false},
	} {
		if got := runtime.Retryable(c.err); got != c.want {
			t.Errorf("Retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
