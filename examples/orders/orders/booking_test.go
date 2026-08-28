package orders_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/runtime"

	"example.com/orders/store/booking"
)

func at(h int) time.Time {
	return time.Date(2026, 6, 1, h, 0, 0, 0, time.UTC)
}

// The scheduling case. Two bookings for the same room that overlap in time
// cannot both exist — enforced by the database, so a race cannot beat it.
func TestBookingOverlapIsRefusedByTheDatabase(t *testing.T) {
	ctx := context.Background()

	first := booking.Create()
	first.SetRoom(101)
	first.SetGuest("Ada")
	first.SetDuring(storm.NewTstzRange(at(9), at(11)))
	if _, err := first.Insert(ctx, ex); err != nil {
		t.Fatal(err)
	}

	// Overlaps 09:00–11:00.
	clash := booking.Create()
	clash.SetRoom(101)
	clash.SetGuest("Grace")
	clash.SetDuring(storm.NewTstzRange(at(10), at(12)))
	_, err := clash.Insert(ctx, ex)
	if err == nil {
		t.Fatal("an overlapping booking was accepted; the exclusion constraint did nothing")
	}
	if !errors.Is(err, runtime.ErrExclusionViolation) {
		t.Fatalf("errors.Is(err, ErrExclusionViolation) is false: %v", err)
	}

	// Half-open: 11:00–12:00 ABUTS 09:00–11:00 and does not overlap it.
	abut := booking.Create()
	abut.SetRoom(101)
	abut.SetGuest("Grace")
	abut.SetDuring(storm.NewTstzRange(at(11), at(12)))
	if _, err := abut.Insert(ctx, ex); err != nil {
		t.Fatalf("an abutting booking was refused; [09,11) and [11,12) do not overlap: %v", err)
	}

	// A different room at the same time is fine.
	other := booking.Create()
	other.SetRoom(102)
	other.SetGuest("Grace")
	other.SetDuring(storm.NewTstzRange(at(10), at(12)))
	if _, err := other.Insert(ctx, ex); err != nil {
		t.Fatalf("a different room was refused: %v", err)
	}
}

// The range survives a round trip through the wire, bounds and all.
func TestRangeRoundTrips(t *testing.T) {
	ctx := context.Background()
	want := storm.NewTstzRange(at(14), at(16))

	n := booking.Create()
	n.SetRoom(201)
	n.SetGuest("Round")
	n.SetDuring(want)
	got, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if !got.During.Lower.Equal(want.Lower) || !got.During.Upper.Equal(want.Upper) {
		t.Errorf("bounds changed: %v..%v, want %v..%v",
			got.During.Lower, got.During.Upper, want.Lower, want.Upper)
	}
	// Half-open is the default and must survive: [inclusive, exclusive).
	if !got.During.LowerInc {
		t.Error("the lower bound came back exclusive; [lower, upper) is the default")
	}
	if got.During.UpperInc {
		t.Error("the upper bound came back inclusive")
	}
	if got.During.Empty {
		t.Error("a non-empty range came back empty")
	}
}

// Overlaps is answered by the database with the same semantics the Go type
// uses, so a check on either side cannot disagree with the other.
func TestOverlapsPredicateAgreesWithGo(t *testing.T) {
	ctx := context.Background()
	room := int32(301)
	for _, r := range []storm.TstzRange{
		storm.NewTstzRange(at(1), at(3)),
		storm.NewTstzRange(at(5), at(7)),
	} {
		n := booking.Create()
		n.SetRoom(room)
		n.SetGuest("Q")
		n.SetDuring(r)
		if _, err := n.Insert(ctx, ex); err != nil {
			t.Fatal(err)
		}
	}

	probe := storm.NewTstzRange(at(2), at(6))
	hits, err := booking.New().
		Where(booking.Room.Eq(room), booking.During.Overlaps(probe)).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("the database found %d overlaps, want 2", len(hits))
	}
	// The Go implementation must agree, row for row.
	for _, h := range hits {
		if !h.During.Overlaps(probe) {
			t.Errorf("the database says %v overlaps %v and Go says it does not",
				h.During, probe)
		}
	}

	// A probe that touches but does not overlap: [3,5) abuts both.
	none, err := booking.New().
		Where(booking.Room.Eq(room), booking.During.Overlaps(storm.NewTstzRange(at(3), at(5)))).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("an abutting probe matched %d rows; [3,5) touches [1,3) and [5,7) but overlaps neither", len(none))
	}
}

// The Go overlap semantics, in isolation — these are the boundary cases that
// hand-rolled timestamp comparisons get wrong.
func TestGoOverlapSemantics(t *testing.T) {
	half := storm.NewTstzRange(at(9), at(11))
	for _, c := range []struct {
		name string
		o    storm.TstzRange
		want bool
	}{
		{"identical", storm.NewTstzRange(at(9), at(11)), true},
		{"contained", storm.NewTstzRange(at(9), at(10)), true},
		{"straddles the end", storm.NewTstzRange(at(10), at(12)), true},
		{"abuts after", storm.NewTstzRange(at(11), at(12)), false},
		{"abuts before", storm.NewTstzRange(at(8), at(9)), false},
		{"disjoint", storm.NewTstzRange(at(13), at(14)), false},
		{"empty", storm.TstzRange{Empty: true}, false},
		{"unbounded after", runtime.Since(at(10)), true},
		{"unbounded before", runtime.Until(at(10)), true},
		{"unbounded before, abutting", runtime.Until(at(9)), false},
	} {
		if got := half.Overlaps(c.o); got != c.want {
			t.Errorf("%s: [9,11).Overlaps(%v) = %v, want %v", c.name, c.o, got, c.want)
		}
	}
}
