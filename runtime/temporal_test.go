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
