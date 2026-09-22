// Package valdec decodes a driver.Value into the Go types a generated scanner
// holds.
//
// The FOURTH decoder family, and the first that reads no bytes. The other three
// — runtime's own, mydec and msdec — take wire bytes and a slab; this one takes
// what a database/sql driver already decoded, which is one of: int64, float64,
// bool, []byte, string, time.Time, or nil.
//
// WHY IT EXISTS. A database/sql driver decodes before storm can see the wire,
// so an adapter over one cannot supply runtime.Rows.RawValues without
// re-encoding what it just decoded. The port grew a second row shape for that
// (see runtime.Rows), and this is the decoder side of it. What it buys is wider
// than the target that forced it: any database/sql driver can now be adapted.
//
// EVERY MAPPING HERE WAS MEASURED, not assumed —
// internal/oraclespike/valuetypes_test.go, 2026-09-23, go-ora against Oracle
// Free 23. The result that decided the design: every Oracle NUMBER arrives as
// an exact decimal STRING, including 2^53+1 and a 34-significant-digit value.
// A float64 could not hold either, so a value path is lossless here in a way it
// would not be against a driver that yields float64 for a numeric.
//
// THE COST, stated plainly. A NUMBER(19) column is storm's int8 and arrives as
// text, so reading it is a ParseInt rather than a load — and that is a large
// part of the 26.3 allocations per row the Oracle spike measured against 0.09
// for a hand-written client. This family is the honest way to support a driver
// storm did not write; it is not a way to make one fast.
package valdec

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// Int8 reads a signed integer.
//
// The string case is not a fallback: it is the COMMON one. Oracle's NUMBER
// arrives as decimal text through go-ora, which is what makes a key past 2^53
// survive — a driver that yielded float64 would have rounded it before storm
// was asked.
func Int8(v any) int64 {
	switch x := v.(type) {
	case nil:
		return 0
	case int64:
		return x
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case []byte:
		n, _ := strconv.ParseInt(string(x), 10, 64)
		return n
	case float64:
		return int64(x)
	}
	return 0
}

// Int4 and Int2 narrow, which is safe because the DDL declared the width: a
// NUMBER(10) cannot hold more than an int32 and a NUMBER(5) cannot hold more
// than an int16.
func Int4(v any) int32 { return int32(Int8(v)) }
func Int2(v any) int16 { return int16(Int8(v)) }

// Float8 reads a binary double.
func Float8(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case []byte:
		f, _ := strconv.ParseFloat(string(x), 64)
		return f
	}
	return 0
}

func Float4(v any) float32 {
	if x, ok := v.(float32); ok {
		return x
	}
	return float32(Float8(v))
}

// Bool reads a boolean.
//
// The STRING case is Oracle's, and it is a finding rather than defensiveness:
// go-ora reports a 23c native BOOLEAN column as database type NUMBER and hands
// back "1" or "0". A decoder that only handled the bool case would read every
// true as false, silently, for every row.
func Bool(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case int64:
		return x != 0
	case string:
		return x == "1" || x == "t" || x == "T" || x == "true" || x == "TRUE" ||
			x == "Y" || x == "y"
	case []byte:
		return Bool(string(x))
	}
	return false
}

// Str reads text. No slab: the driver already allocated the string, so copying
// it into one would be a second allocation for no lifetime benefit — unlike the
// byte families, where the slab is what keeps a wire buffer from being reused
// underneath a live row.
func Str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	}
	return fmt.Sprint(v)
}

// Bytes reads binary. The driver's slice is COPIED, because a database/sql
// driver is free to reuse its buffer on the next Next — the same hazard the
// byte families answer with a slab.
func Bytes(v any) []byte {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return append([]byte(nil), x...)
	case string:
		return []byte(x)
	}
	return nil
}

// Time reads a temporal.
func Time(v any) (time.Time, error) {
	switch x := v.(type) {
	case nil:
		return time.Time{}, nil
	case time.Time:
		return x, nil
	case string:
		return parseTime(x)
	case []byte:
		return parseTime(string(x))
	}
	return time.Time{}, fmt.Errorf("valdec: cannot read %T as a time", v)
}

// parseTime handles a driver that hands temporals back as text. go-ora does
// not — it yields time.Time, with the offset preserved for a TIMESTAMP WITH
// TIME ZONE — but a value adapter is meant to serve more than one driver, and
// a text temporal is common enough that failing on it would be a surprise.
func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -07:00",
		"2006-01-02 15:04:05.999999999", "2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("valdec: cannot parse %q as a time", s)
}

// Decimal reads an exact number.
//
// Through its decimal STRING, always, and never through a float64. This is the
// measurement that decided the whole family: an Oracle NUMBER(19,4) arrives as
// "12345.6789", and a money column routed through a float64 is a money column
// with a rounding error that nothing downstream can undo.
func Decimal(v any) (runtime.Decimal, error) {
	switch x := v.(type) {
	case nil:
		return runtime.Decimal{}, nil
	case string:
		return runtime.ParseDecimal(x)
	case []byte:
		return runtime.ParseDecimal(string(x))
	case int64:
		return runtime.ParseDecimal(strconv.FormatInt(x, 10))
	case float64:
		// Reachable only through a driver that already lost the precision.
		// Named rather than silently accepted, because the loss happened
		// BEFORE storm was asked and the error is the only place to say so.
		return runtime.Decimal{}, fmt.Errorf(
			"valdec: this driver decoded an exact numeric as a float64 (%v), which has "+
				"already lost precision — storm cannot recover it", x)
	}
	return runtime.Decimal{}, fmt.Errorf("valdec: cannot read %T as a decimal", v)
}

// IsNull reports whether a value is SQL NULL, which is how a generated scanner
// decides between a pointer field's nil and its value.
func IsNull(v any) bool { return v == nil }
