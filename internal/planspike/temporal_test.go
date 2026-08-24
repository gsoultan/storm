package planspike_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/gsoultan/raorm"
	"github.com/gsoultan/raorm/internal/planspike/store/event"
)

// A calendar date survives the trip exactly, and compares as one.
func TestDate_RoundTripAndOrdering(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	days := []string{"2026-03-01", "2026-01-15", "2026-02-01"}
	var ids [][16]byte
	for _, d := range days {
		on, err := time.Parse("2006-01-02", d)
		if err != nil {
			t.Fatal(err)
		}
		n := event.Create()
		n.SetOn(on)
		n.SetAddr(netip.MustParsePrefix("10.0.0.1/32"))
		n.SetNet(netip.MustParsePrefix("10.0.0.0/24"))
		n.SetTags([]int64{})
		r, err := n.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_ = event.Delete(ctx, ex, id)
		}
	})

	rows, err := event.New().Where(event.ID.In(ids...)).Order(event.On.Asc()).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d events, want 3", len(rows))
	}
	want := []string{"2026-01-15", "2026-02-01", "2026-03-01"}
	for i, r := range rows {
		if got := r.On.Format("2006-01-02"); got != want[i] {
			t.Errorf("position %d: %s, want %s", i, got, want[i])
		}
		if h, m, s := r.On.Clock(); h+m+s != 0 {
			t.Errorf("a date came back with a time of day: %v", r.On)
		}
	}

	// And a range predicate: dates compare like the calendar says.
	cut, _ := time.Parse("2006-01-02", "2026-02-01")
	n, err := event.New().Where(event.ID.In(ids...), event.On.Gte(cut)).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("Gte(feb 1) matched %d, want 2", n)
	}
}

// An interval keeps months, days and micros APART — that is the whole design.
func TestInterval_RoundTripKeepsFieldsApart(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	iv := raorm.Interval{Months: 13, Days: 2, Micros: int64(90 * time.Minute / time.Microsecond)}
	n := event.Create()
	n.SetOn(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	n.SetWindow(iv)
	n.SetAddr(netip.MustParsePrefix("10.0.0.2/32"))
	n.SetNet(netip.MustParsePrefix("10.1.0.0/16"))
	n.SetTags([]int64{})
	r, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = event.Delete(ctx, ex, r.ID) })

	got, ok := r.Window.Get()
	if !ok {
		t.Fatal("the interval came back NULL")
	}
	if got != iv {
		t.Errorf("interval = %+v, want %+v — months and days must not be flattened", got, iv)
	}
	if _, ok := got.Duration(); ok {
		t.Error("13 months flattened to a Duration; a month has no length")
	}
}

// inet keeps host bits; cidr is the database rejecting them.
func TestInet_HostBitsAndCidrEnforcement(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	addr := netip.MustParsePrefix("192.168.7.42/24") // host bits, legal for inet
	v6 := netip.MustParsePrefix("2001:db8::7/64")

	n := event.Create()
	n.SetOn(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	n.SetAddr(addr)
	n.SetNet(netip.MustParsePrefix("192.168.7.0/24"))
	n.SetTags([]int64{})
	r, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = event.Delete(ctx, ex, r.ID) })
	if got, _ := r.Addr, false; got != addr {
		t.Errorf("inet = %s, want %s — host bits must survive", got, addr)
	}

	// Find it BY address: the predicate binds a netip.Prefix.
	cnt, err := event.New().Where(event.Addr.Eq(addr)).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("Eq(%s) matched %d rows, want 1", addr, cnt)
	}

	// v6 through the same column.
	m := event.Mutate(r)
	m.SetAddr(v6)
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	back, _, err := event.New().Where(event.ID.Eq(r.ID)).One(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if back.Addr != v6 {
		t.Errorf("v6 inet = %s, want %s", back.Addr, v6)
	}

	// cidr: host bits are the DATABASE's to reject, and it does.
	bad := event.Create()
	bad.SetOn(time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	bad.SetAddr(addr)
	bad.SetNet(netip.MustParsePrefix("10.0.0.1/24")) // host bits in a cidr
	bad.SetTags([]int64{})
	if _, err := bad.Insert(ctx, ex); err == nil {
		t.Error("a cidr with host bits must be rejected — that is the entire difference from inet")
	}
}

// int8[] rides the same array machinery as text[].
func TestInt8Array_RoundTrip(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	n := event.Create()
	n.SetOn(time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC))
	n.SetAddr(netip.MustParsePrefix("10.0.0.9/32"))
	n.SetNet(netip.MustParsePrefix("10.9.0.0/16"))
	n.SetTags([]int64{-1, 0, 9_223_372_036_854_775_807})
	r, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = event.Delete(ctx, ex, r.ID) })

	if len(r.Tags) != 3 || r.Tags[0] != -1 || r.Tags[2] != 9_223_372_036_854_775_807 {
		t.Errorf("tags = %v — int64 range must survive", r.Tags)
	}
}
