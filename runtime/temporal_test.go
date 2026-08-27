package runtime_test

import (
	"net/netip"
	"testing"
	"time"

	"github.com/gsoultan/raorm/runtime"
)

func TestDate_Decodes(t *testing.T) {
	// days since 2000-01-01, big-endian int32
	for _, tc := range []struct {
		days int32
		want string
	}{
		{0, "2000-01-01"},
		{1, "2000-01-02"},
		{-1, "1999-12-31"},
		{9497, "2026-01-01"},
	} {
		b := []byte{byte(tc.days >> 24), byte(tc.days >> 16), byte(tc.days >> 8), byte(tc.days)}
		got := runtime.Date(b)
		if got.Format("2006-01-02") != tc.want {
			t.Errorf("day %d = %s, want %s", tc.days, got.Format("2006-01-02"), tc.want)
		}
		if got.Location() != time.UTC {
			t.Errorf("day %d decoded in %v; the convention is UTC midnight", tc.days, got.Location())
		}
	}
}

func TestInterval_RoundTripAndDuration(t *testing.T) {
	iv := runtime.Interval{Months: 14, Days: 3, Micros: 90061000000} // 1y2m 3d 25:01:01
	got, err := runtime.IntervalErr(runtime.EncodeInterval(iv, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got != iv {
		t.Errorf("round-tripped to %+v", got)
	}

	// Months present: no honest Duration exists.
	if _, ok := iv.Duration(); ok {
		t.Error("an interval with months flattened to a Duration — a month has no length")
	}
	flat := runtime.Interval{Days: 2, Micros: int64(3 * time.Hour / time.Microsecond)}
	d, ok := flat.Duration()
	if !ok || d != 51*time.Hour {
		t.Errorf("2 days 3h = %v ok=%v, want 51h", d, ok)
	}
}

func TestInterval_ShortInputIsAnError(t *testing.T) {
	if _, err := runtime.IntervalErr([]byte{1, 2, 3}); err == nil {
		t.Error("a truncated interval must be an error")
	}
	if iv, err := runtime.IntervalErr(nil); err != nil || iv != (runtime.Interval{}) {
		t.Error("nil is SQL NULL and decodes to the zero interval with no error")
	}
}

func TestInet_Decodes(t *testing.T) {
	// family, bits, is_cidr, addrlen, addr...
	v4 := append([]byte{2, 24, 0, 4}, 192, 168, 1, 7)
	p, err := runtime.InetErr(v4)
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != "192.168.1.7/24" {
		t.Errorf("v4 = %s", p)
	}

	v6addr := netip.MustParseAddr("2001:db8::1").As16()
	v6 := append([]byte{3, 64, 1, 16}, v6addr[:]...)
	p, err = runtime.InetErr(v6)
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != "2001:db8::1/64" {
		t.Errorf("v6 = %s", p)
	}
}

func TestInet_MalformedIsAnError(t *testing.T) {
	for _, b := range [][]byte{
		{2, 24, 0},                   // too short
		{2, 24, 0, 4, 1, 2},          // truncated address
		{2, 24, 0, 5, 1, 2, 3, 4, 5}, // 5-byte address
		{2, 33, 0, 4, 1, 2, 3, 4},    // /33 on v4
	} {
		if _, err := runtime.InetErr(b); err == nil {
			t.Errorf("%v decoded without error", b)
		}
	}
}

func TestNullDecoders_NullAndValue(t *testing.T) {
	// nil is SQL NULL for every nullable decoder; a value decodes as itself.
	if v, err := runtime.NullInet(nil); err != nil || v.Valid {
		t.Errorf("NullInet(nil) = %+v, %v", v, err)
	}
	v4 := append([]byte{2, 32, 0, 4}, 10, 0, 0, 7)
	if v, err := runtime.NullInet(v4); err != nil || !v.Valid || v.V.String() != "10.0.0.7/32" {
		t.Errorf("NullInet(v4) = %+v, %v", v, err)
	}
	if _, err := runtime.NullInet([]byte{2}); err == nil {
		t.Error("a truncated nullable inet must error, not read as NULL")
	}

	var sl runtime.Slab
	if v := runtime.NullJSON(nil, &sl); v.Valid {
		t.Error("NullJSON(nil) is not NULL")
	}
	if v := runtime.NullJSON([]byte{1, '{', '}'}, &sl); !v.Valid || v.V.String() != "{}" {
		t.Errorf("NullJSON = %+v", v)
	}

	// Numeric's zero-on-error contract: the generated scanner checks the error
	// path separately; the value returned alongside an error is the zero.
	if d := runtime.Numeric([]byte{0, 0, 0, 0, 0xC0, 0x00, 0, 0}); d != (runtime.Decimal{}) {
		t.Errorf("Numeric(NaN) leaked a value: %+v", d)
	}
	if d := runtime.Numeric(runtime.EncodeNumeric(runtime.Decimal{Unscaled: 7, Scale: 1}, nil)); d.String() != "0.7" {
		t.Errorf("Numeric = %s", d)
	}
}

func TestTimeOfDay_PartsAndString(t *testing.T) {
	for _, tc := range []struct {
		h, m, s, us int
		want        string
	}{
		{0, 0, 0, 0, "00:00:00"},
		{9, 5, 3, 0, "09:05:03"},
		{23, 59, 59, 999999, "23:59:59.999999"},
		{12, 0, 0, 500000, "12:00:00.5"}, // trailing zeros trimmed
		{12, 0, 0, 1, "12:00:00.000001"}, // and not over-trimmed
		{13, 45, 30, 120000, "13:45:30.12"},
	} {
		v, ok := runtime.NewTimeOfDay(tc.h, tc.m, tc.s, tc.us)
		if !ok {
			t.Fatalf("NewTimeOfDay(%d,%d,%d,%d) rejected a legal time", tc.h, tc.m, tc.s, tc.us)
		}
		if got := v.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
		h, m, s, us := v.Parts()
		if h != tc.h || m != tc.m || s != tc.s || us != tc.us {
			t.Errorf("Parts() = %d,%d,%d,%d, want %d,%d,%d,%d", h, m, s, us, tc.h, tc.m, tc.s, tc.us)
		}
	}

	// 24:00:00 is a legal PostgreSQL time and the boundary a range check gets
	// wrong; one microsecond past it is not.
	if got := runtime.MaxTimeOfDay.String(); got != "24:00:00" {
		t.Errorf("MaxTimeOfDay = %q", got)
	}
	if _, ok := runtime.NewTimeOfDay(24, 0, 0, 0); !ok {
		t.Error("24:00:00 must be accepted — PostgreSQL accepts it")
	}
	if _, ok := runtime.NewTimeOfDay(24, 0, 0, 1); ok {
		t.Error("one microsecond past 24:00:00 must be rejected")
	}
}

// Out-of-range parts are rejected rather than normalised: 25:00 meaning 01:00
// tomorrow would be a different fact silently substituted for the one asked
// for.
func TestTimeOfDay_RejectsOutOfRangeParts(t *testing.T) {
	for _, tc := range [][4]int{
		{25, 0, 0, 0}, {-1, 0, 0, 0}, {0, 60, 0, 0}, {0, -1, 0, 0},
		{0, 0, 60, 0}, {0, 0, -1, 0}, {0, 0, 0, 1000000}, {0, 0, 0, -1},
	} {
		if _, ok := runtime.NewTimeOfDay(tc[0], tc[1], tc[2], tc[3]); ok {
			t.Errorf("NewTimeOfDay%v was accepted", tc)
		}
	}
}

func TestTimeOfDay_Duration(t *testing.T) {
	v, _ := runtime.NewTimeOfDay(1, 30, 0, 0)
	if got := v.Duration(); got != 90*time.Minute {
		t.Errorf("Duration() = %v, want 90m", got)
	}
	if got := runtime.TimeOfDay(0).Duration(); got != 0 {
		t.Errorf("midnight Duration() = %v", got)
	}
}

func TestTimeOfDay_WireRoundTrip(t *testing.T) {
	for _, v := range []runtime.TimeOfDay{0, 1, 34200000000, runtime.MaxTimeOfDay} {
		buf := runtime.EncodeTimeOfDay(v, nil)
		if len(buf) != 8 {
			t.Fatalf("encoded %s to %d bytes, want 8", v, len(buf))
		}
		got, err := runtime.TimeOfDayErr(buf)
		if err != nil || got != v {
			t.Fatalf("round trip of %s gave %s (err %v)", v, got, err)
		}
		n, err := runtime.NullTimeOfDay(buf)
		if err != nil || !n.Valid || n.V != v {
			t.Fatalf("NullTimeOfDay round trip of %s gave %v (err %v)", v, n, err)
		}
	}

	// NULL and a short value are different failures: one is a fact, the other
	// is a corrupt wire value.
	if v, err := runtime.TimeOfDayErr(nil); err != nil || v != 0 {
		t.Errorf("NULL should decode as the zero value without error, got %s / %v", v, err)
	}
	if n, err := runtime.NullTimeOfDay(nil); err != nil || n.Valid {
		t.Errorf("NULL should decode as invalid without error, got %v / %v", n, err)
	}
	if _, err := runtime.TimeOfDayErr([]byte{1, 2, 3}); err == nil {
		t.Error("a short wire value must be an error, not a truncated time")
	}
	if _, err := runtime.NullTimeOfDay([]byte{1, 2, 3}); err == nil {
		t.Error("a short wire value must be an error through the nullable path too")
	}
}
