package runtime

import (
	"encoding/binary"
	"errors"
	"time"
)

// TstzRange is a PostgreSQL `tstzrange`: an interval of time with explicit
// bounds.
//
// The type exists so that "does this booking overlap that one" is a question
// the database answers with an index, rather than four comparisons in Go that
// get the boundary cases wrong. `[09:00, 10:00)` and `[10:00, 11:00)` do NOT
// overlap; `[09:00, 10:00]` and `[10:00, 11:00)` do. Storing two timestamps and
// comparing them by hand is where that distinction gets lost.
//
// The zero value is the empty range, which overlaps nothing.
type TstzRange struct {
	Lower, Upper time.Time
	// LowerInc and UpperInc are the bound inclusivities. PostgreSQL's default
	// and the sane choice for scheduling is half-open — `[lower, upper)` —
	// which is what NewTstzRange builds: two adjacent bookings then do not
	// collide on the instant they touch.
	LowerInc, UpperInc bool
	// LowerInf and UpperInf are unbounded ends: "from then on", "until then".
	LowerInf, UpperInf bool
	// Empty is the empty range. It contains nothing and overlaps nothing, and
	// is NOT the same as a range whose bounds happen to be equal.
	Empty bool
}

// NewTstzRange builds the half-open range [lower, upper), which is the one
// scheduling wants: adjacent slots do not overlap.
func NewTstzRange(lower, upper time.Time) TstzRange {
	return TstzRange{Lower: lower, Upper: upper, LowerInc: true}
}

// Since is the unbounded range [t, ∞).
func Since(t time.Time) TstzRange {
	return TstzRange{Lower: t, LowerInc: true, UpperInf: true}
}

// Until is the unbounded range (-∞, t).
func Until(t time.Time) TstzRange {
	return TstzRange{Upper: t, LowerInf: true}
}

// IsEmpty reports whether the range contains nothing.
func (r TstzRange) IsEmpty() bool { return r.Empty }

// Range wire-format flag bits, as PostgreSQL sends them.
const (
	rangeEmpty     = 0x01
	rangeLowerInc  = 0x02
	rangeUpperInc  = 0x04
	rangeLowerInf  = 0x08
	rangeUpperInf  = 0x10
	rangeLowerNull = 0x20
	rangeUpperNull = 0x40
)

// ErrRangeFormat is a range whose wire value cannot be read. Like every other
// decode error it names the shape and never the value.
var ErrRangeFormat = errors.New("storm: tstzrange wire value is malformed")

// DecodeTstzRange reads the binary wire format.
//
// One flags byte, then each present bound as a 4-byte length and 8 bytes of
// microseconds since the PostgreSQL epoch. The infinite and empty cases carry
// no bound at all, which is why the flags have to be read first rather than the
// length being trusted.
func DecodeTstzRange(b []byte) (TstzRange, error) {
	if len(b) < 1 {
		return TstzRange{}, ErrRangeFormat
	}
	flags := b[0]
	if flags&rangeEmpty != 0 {
		return TstzRange{Empty: true}, nil
	}
	r := TstzRange{
		LowerInc: flags&rangeLowerInc != 0,
		UpperInc: flags&rangeUpperInc != 0,
		LowerInf: flags&rangeLowerInf != 0,
		UpperInf: flags&rangeUpperInf != 0,
	}
	at := 1
	read := func() (time.Time, error) {
		if at+4 > len(b) {
			return time.Time{}, ErrRangeFormat
		}
		n := int(int32(binary.BigEndian.Uint32(b[at : at+4])))
		at += 4
		if n != 8 || at+8 > len(b) {
			return time.Time{}, ErrRangeFormat
		}
		t := Timestamptz(b[at : at+8])
		at += 8
		return t, nil
	}
	var err error
	if !r.LowerInf && flags&rangeLowerNull == 0 {
		if r.Lower, err = read(); err != nil {
			return TstzRange{}, err
		}
	}
	if !r.UpperInf && flags&rangeUpperNull == 0 {
		if r.Upper, err = read(); err != nil {
			return TstzRange{}, err
		}
	}
	return r, nil
}

// EncodeTstzRange writes the binary wire format, appending to buf.
func EncodeTstzRange(r TstzRange, buf []byte) []byte {
	if r.Empty {
		return append(buf, rangeEmpty)
	}
	var flags byte
	if r.LowerInc {
		flags |= rangeLowerInc
	}
	if r.UpperInc {
		flags |= rangeUpperInc
	}
	if r.LowerInf {
		flags |= rangeLowerInf
	}
	if r.UpperInf {
		flags |= rangeUpperInf
	}
	buf = append(buf, flags)
	put := func(t time.Time) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], 8)
		buf = append(buf, n[:]...)
		var v [8]byte
		binary.BigEndian.PutUint64(v[:], uint64(t.Sub(PGEpoch)/time.Microsecond))
		buf = append(buf, v[:]...)
	}
	if !r.LowerInf {
		put(r.Lower)
	}
	if !r.UpperInf {
		put(r.Upper)
	}
	return buf
}

// Overlaps reports whether two ranges share any instant — the same question
// PostgreSQL's `&&` asks, answered identically so a Go-side check and a
// database-side one cannot disagree.
func (r TstzRange) Overlaps(o TstzRange) bool {
	if r.Empty || o.Empty {
		return false
	}
	return !r.before(o) && !o.before(r)
}

// before reports whether r ends at or before o begins, with no shared instant.
func (r TstzRange) before(o TstzRange) bool {
	if r.UpperInf || o.LowerInf {
		return false
	}
	if r.Upper.Before(o.Lower) {
		return true
	}
	if r.Upper.Equal(o.Lower) {
		// They touch. Shared only when BOTH bounds include the instant.
		return !(r.UpperInc && o.LowerInc)
	}
	return false
}

// Contains reports whether an instant falls inside the range.
func (r TstzRange) Contains(t time.Time) bool {
	if r.Empty {
		return false
	}
	if !r.LowerInf {
		if t.Before(r.Lower) || (t.Equal(r.Lower) && !r.LowerInc) {
			return false
		}
	}
	if !r.UpperInf {
		if t.After(r.Upper) || (t.Equal(r.Upper) && !r.UpperInc) {
			return false
		}
	}
	return true
}

// TstzRangeErr decodes a non-null tstzrange.
func TstzRangeErr(b []byte) (TstzRange, error) { return DecodeTstzRange(b) }

// NullTstzRange decodes a nullable tstzrange.
func NullTstzRange(b []byte) (Null[TstzRange], error) {
	if b == nil {
		return Null[TstzRange]{}, nil
	}
	r, err := DecodeTstzRange(b)
	if err != nil {
		return Null[TstzRange]{}, err
	}
	return Null[TstzRange]{V: r, Valid: true}, nil
}
