// Package mydec decodes MySQL's binary result protocol.
//
// The sibling of the PostgreSQL decoders in `runtime`, and deliberately a
// separate package rather than a branch inside them: which family a generated
// scanner calls is decided at GENERATE time, so there is no dialect test on the
// hot path — the same reason `compile/` is split by back end.
//
// Nothing here is shared with the PostgreSQL family, because nothing can be.
// MySQL is **little-endian** where PostgreSQL is big-endian, and its temporal
// types are packed component-wise rather than as an offset from an epoch.
// `binary.BigEndian.Uint64` on a MySQL BIGINT reads a byte-reversed number
// without erroring — which is why these are different functions with different
// names rather than a flag (ADR-0007).
//
// Same budget as the PostgreSQL family: no allocation except the string copies
// a Go string requires, and no `any` anywhere.
package mydec

import (
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// ErrFormat is a value whose wire encoding is not the length the type requires.
// Like every storm decode error it names the shape and never the value.
var ErrFormat = errors.New("storm/mydec: MySQL binary value has the wrong length")

// ---- integers ---------------------------------------------------------------
//
// Fixed width, little-endian. MySQL sends a TINYINT as one byte, a SMALLINT as
// two, and so on; there is no length prefix inside a binary result row.

func Bool(b []byte) bool { return len(b) > 0 && b[0] != 0 }

func Int1(b []byte) int8 {
	if len(b) < 1 {
		return 0
	}
	return int8(b[0])
}

func Int2(b []byte) int16 {
	if len(b) < 2 {
		return 0
	}
	return int16(binary.LittleEndian.Uint16(b))
}

func Int4(b []byte) int32 {
	if len(b) < 4 {
		return 0
	}
	return int32(binary.LittleEndian.Uint32(b))
}

func Int8(b []byte) int64 {
	if len(b) < 8 {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(b))
}

func Float4(b []byte) float32 {
	if len(b) < 4 {
		return 0
	}
	return math.Float32frombits(binary.LittleEndian.Uint32(b))
}

func Float8(b []byte) float64 {
	if len(b) < 8 {
		return 0
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b))
}

// ---- strings and bytes ------------------------------------------------------

// Text copies, because the wire buffer is reused on the next row. Same contract
// as the PostgreSQL family and the same single allocation.
func Text(b []byte) string { return string(b) }

func Bytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// ---- temporal ---------------------------------------------------------------
//
// MySQL packs its temporal types component-wise, with a LEADING LENGTH that
// says how much detail is present: a DATETIME with no time part is 4 bytes, one
// with seconds is 7, one with microseconds is 11. A decoder that assumed the
// widest form would read past the value on the common case, so the length is
// read first and every shorter form is legal.

// DateTime reads a DATE, DATETIME or TIMESTAMP.
//
// Returned in UTC. MySQL's DATETIME carries no zone — it stores what it was
// given — and storm writes UTC, so reading it back as UTC is the round trip.
// Interpreting it in time.Local would make the value depend on where the
// process runs, which is the class of bug that only shows up after a deploy.
func DateTime(b []byte) (time.Time, error) {
	if len(b) == 0 {
		return time.Time{}, nil // the zero date
	}
	n := int(b[0])
	if len(b) < n+1 {
		return time.Time{}, ErrFormat
	}
	switch n {
	case 0:
		return time.Time{}, nil
	case 4, 7, 11:
	default:
		return time.Time{}, ErrFormat
	}
	year := int(binary.LittleEndian.Uint16(b[1:3]))
	month := time.Month(b[3])
	day := int(b[4])
	var hour, min, sec, micro int
	if n >= 7 {
		hour, min, sec = int(b[5]), int(b[6]), int(b[7])
	}
	if n == 11 {
		micro = int(binary.LittleEndian.Uint32(b[8:12]))
	}
	return time.Date(year, month, day, hour, min, sec, micro*1000, time.UTC), nil
}

// Date reads a DATE as a time.Time at midnight UTC.
func Date(b []byte) (time.Time, error) { return DateTime(b) }

// Duration reads a TIME, which MySQL models as a signed duration rather than a
// time of day — it ranges beyond ±24h on purpose.
func Duration(b []byte) (time.Duration, error) {
	if len(b) == 0 {
		return 0, nil
	}
	n := int(b[0])
	if len(b) < n+1 {
		return 0, ErrFormat
	}
	switch n {
	case 0:
		return 0, nil
	case 8, 12:
	default:
		return 0, ErrFormat
	}
	neg := b[1] != 0
	days := int64(binary.LittleEndian.Uint32(b[2:6]))
	d := time.Duration(days)*24*time.Hour +
		time.Duration(b[6])*time.Hour +
		time.Duration(b[7])*time.Minute +
		time.Duration(b[8])*time.Second
	if n == 12 {
		d += time.Duration(binary.LittleEndian.Uint32(b[9:13])) * time.Microsecond
	}
	if neg {
		d = -d
	}
	return d, nil
}

// ---- decimal ----------------------------------------------------------------

// Decimal reads a DECIMAL.
//
// MySQL sends DECIMAL as TEXT even in the binary protocol — it is a
// length-encoded string of digits, not a packed number. So this parses digits,
// which is also why it cannot overflow silently the way a fixed-width read
// could: too many significant digits is an error, not a truncation.
func Decimal(b []byte) (unscaled int64, scale int32, err error) {
	if len(b) == 0 {
		return 0, 0, nil
	}
	neg := false
	i := 0
	if b[0] == '-' {
		neg, i = true, 1
	} else if b[0] == '+' {
		i = 1
	}
	seenDot := false
	var digits int
	for ; i < len(b); i++ {
		c := b[i]
		if c == '.' {
			if seenDot {
				return 0, 0, ErrFormat
			}
			seenDot = true
			continue
		}
		if c < '0' || c > '9' {
			return 0, 0, ErrFormat
		}
		if digits == 18 {
			// Same ceiling, and the same reason, as the PostgreSQL family: an
			// int64 unscaled value holds 18 significant digits. Nineteen is
			// refused loudly rather than wrapped quietly.
			return 0, 0, ErrDecimalRange
		}
		unscaled = unscaled*10 + int64(c-'0')
		digits++
		if seenDot {
			scale++
		}
	}
	if neg {
		unscaled = -unscaled
	}
	return unscaled, scale, nil
}

// ErrDecimalRange is a DECIMAL with more significant digits than an int64
// unscaled value can hold.
var ErrDecimalRange = errors.New(
	"storm/mydec: DECIMAL has more than 18 significant digits, which a Decimal cannot hold")

// ---- uuid -------------------------------------------------------------------

// UUID reads the BINARY(16) that storm maps a uuid to on MySQL.
//
// Byte order is NOT reversed: a uuid is an opaque 16-byte identifier, not an
// integer, so the little-endian rule that governs every number here does not
// apply to it. Reversing it would produce a different, valid-looking uuid —
// the quietest possible corruption.
func UUID(b []byte) [16]byte {
	var out [16]byte
	if len(b) >= 16 {
		copy(out[:], b[:16])
	}
	return out
}
