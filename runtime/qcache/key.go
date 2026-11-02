package qcache

import (
	"crypto/sha256"
	"database/sql/driver"
	"encoding"
	"encoding/binary"
	"encoding/hex"
	"math"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// keyVersion changes whenever the bytes hashed into a key change meaning, so
// that entries written by an older storm are never read by a newer one.
const keyVersion = 1

// Argument tags. Each Go type gets its own, so int32(1) and int64(1) are two
// keys: a distinct key costs a miss, a shared one could cost a wrong answer.
const (
	argNil byte = iota + 1
	argString
	argBytes
	argJSON
	argBool
	argInt
	argInt8
	argInt16
	argInt32
	argInt64
	argUint
	argUint8
	argUint16
	argUint32
	argUint64
	argFloat32
	argFloat64
	argTime
	argDuration
	argDecimal
	argInterval
	argTimeOfDay
	argTstzRange
	argUUID
	argStrings
	argInt64s
	argInt32s
	argInts
	argBools
	argFloat64s
	argBytesList
	argTimes
	argUUIDs
	argBinary
)

// appendArg appends an unambiguous encoding of v to b. ok is false when v is
// a type the cache cannot encode faithfully, and the call then goes to the
// database uncached: refusing to cache is always correct, guessing is not.
//
// A type switch, because runtime/ may not import reflect — and because
// formatting a value with %v is not an encoding: two distinct values can print
// alike, and a pointer prints as its address.
func appendArg(b []byte, v any, depth int) ([]byte, bool) {
	switch x := v.(type) {
	case nil:
		return append(b, argNil), true
	case string:
		return appendStr(append(b, argString), x), true
	case []byte:
		if x == nil {
			return append(b, argNil), true
		}
		return appendStr(append(b, argBytes), string(x)), true
	case runtime.JSON:
		if x == nil {
			return append(b, argNil), true
		}
		return appendStr(append(b, argJSON), string(x)), true
	case bool:
		if x {
			return append(b, argBool, 1), true
		}
		return append(b, argBool, 0), true
	case int:
		return binary.AppendVarint(append(b, argInt), int64(x)), true
	case int8:
		return binary.AppendVarint(append(b, argInt8), int64(x)), true
	case int16:
		return binary.AppendVarint(append(b, argInt16), int64(x)), true
	case int32:
		return binary.AppendVarint(append(b, argInt32), int64(x)), true
	case int64:
		return binary.AppendVarint(append(b, argInt64), x), true
	case uint:
		return binary.AppendUvarint(append(b, argUint), uint64(x)), true
	case uint8:
		return binary.AppendUvarint(append(b, argUint8), uint64(x)), true
	case uint16:
		return binary.AppendUvarint(append(b, argUint16), uint64(x)), true
	case uint32:
		return binary.AppendUvarint(append(b, argUint32), uint64(x)), true
	case uint64:
		return binary.AppendUvarint(append(b, argUint64), x), true
	case float32:
		return binary.BigEndian.AppendUint32(append(b, argFloat32), math.Float32bits(x)), true
	case float64:
		return binary.BigEndian.AppendUint64(append(b, argFloat64), math.Float64bits(x)), true
	case time.Time:
		return appendTime(append(b, argTime), x)
	case time.Duration:
		return binary.AppendVarint(append(b, argDuration), int64(x)), true
	case runtime.Decimal:
		b = binary.AppendVarint(append(b, argDecimal), x.Unscaled)
		return binary.AppendVarint(b, int64(x.Scale)), true
	case runtime.Interval:
		b = binary.AppendVarint(append(b, argInterval), int64(x.Months))
		b = binary.AppendVarint(b, int64(x.Days))
		return binary.AppendVarint(b, x.Micros), true
	case runtime.TimeOfDay:
		return binary.AppendVarint(append(b, argTimeOfDay), int64(x)), true
	case runtime.TstzRange:
		return appendRange(append(b, argTstzRange), x)
	case [16]byte:
		return append(append(b, argUUID), x[:]...), true

	// Pointers are how a nullable value is bound: nil is NULL.
	case *string:
		if x == nil {
			return append(b, argNil), true
		}
		return appendArg(b, *x, depth)
	case *int64:
		if x == nil {
			return append(b, argNil), true
		}
		return appendArg(b, *x, depth)
	case *int32:
		if x == nil {
			return append(b, argNil), true
		}
		return appendArg(b, *x, depth)
	case *int:
		if x == nil {
			return append(b, argNil), true
		}
		return appendArg(b, *x, depth)
	case *bool:
		if x == nil {
			return append(b, argNil), true
		}
		return appendArg(b, *x, depth)
	case *float64:
		if x == nil {
			return append(b, argNil), true
		}
		return appendArg(b, *x, depth)
	case *time.Time:
		if x == nil {
			return append(b, argNil), true
		}
		return appendArg(b, *x, depth)

	// Lists, which is how an IN over bound values arrives.
	case []string:
		b = binary.AppendUvarint(append(b, argStrings), uint64(len(x)))
		for _, s := range x {
			b = appendStr(b, s)
		}
		return b, true
	case []int64:
		b = binary.AppendUvarint(append(b, argInt64s), uint64(len(x)))
		for _, n := range x {
			b = binary.AppendVarint(b, n)
		}
		return b, true
	case []int32:
		b = binary.AppendUvarint(append(b, argInt32s), uint64(len(x)))
		for _, n := range x {
			b = binary.AppendVarint(b, int64(n))
		}
		return b, true
	case []int:
		b = binary.AppendUvarint(append(b, argInts), uint64(len(x)))
		for _, n := range x {
			b = binary.AppendVarint(b, int64(n))
		}
		return b, true
	case []bool:
		b = binary.AppendUvarint(append(b, argBools), uint64(len(x)))
		for _, t := range x {
			if t {
				b = append(b, 1)
			} else {
				b = append(b, 0)
			}
		}
		return b, true
	case []float64:
		b = binary.AppendUvarint(append(b, argFloat64s), uint64(len(x)))
		for _, f := range x {
			b = binary.BigEndian.AppendUint64(b, math.Float64bits(f))
		}
		return b, true
	case [][]byte:
		b = binary.AppendUvarint(append(b, argBytesList), uint64(len(x)))
		for _, s := range x {
			if s == nil {
				b = append(b, 0)
				continue
			}
			b = appendStr(append(b, 1), string(s))
		}
		return b, true
	case []time.Time:
		b = binary.AppendUvarint(append(b, argTimes), uint64(len(x)))
		for _, t := range x {
			var ok bool
			if b, ok = appendTime(b, t); !ok {
				return b, false
			}
		}
		return b, true
	case [][16]byte:
		b = binary.AppendUvarint(append(b, argUUIDs), uint64(len(x)))
		for _, u := range x {
			b = append(b, u[:]...)
		}
		return b, true

	// What the database itself will see. Bounded, because a Valuer may
	// return another Valuer.
	case driver.Valuer:
		if depth > 2 {
			return b, false
		}
		dv, err := x.Value()
		if err != nil {
			return b, false
		}
		return appendArg(b, dv, depth+1)
	case encoding.BinaryMarshaler:
		raw, err := x.MarshalBinary()
		if err != nil {
			return b, false
		}
		return appendStr(append(b, argBinary), string(raw)), true
	}
	return b, false
}

func appendStr(b []byte, s string) []byte {
	return append(binary.AppendUvarint(b, uint64(len(s))), s...)
}

// appendTime keys the instant AND the zone offset: the same instant bound in
// two zones is two values to a timestamp-without-time-zone column.
func appendTime(b []byte, t time.Time) ([]byte, bool) {
	raw, err := t.MarshalBinary()
	if err != nil {
		return b, false
	}
	return appendStr(b, string(raw)), true
}

func appendRange(b []byte, r runtime.TstzRange) ([]byte, bool) {
	var flags byte
	for i, f := range [...]bool{r.LowerInc, r.UpperInc, r.LowerInf, r.UpperInf, r.Empty} {
		if f {
			flags |= 1 << i
		}
	}
	b = append(b, flags)
	b, ok := appendTime(b, r.Lower)
	if !ok {
		return b, false
	}
	return appendTime(b, r.Upper)
}

// entryKey derives the key of one cached result: a SHA-256 over everything
// that decides which rows come back — the scope, the SQL, every argument, and
// the current generation of every tag. A generation changes on Invalidate,
// which is what makes every entry written before it unreachable.
func entryKey(prefix, scope, sql string, args []any, tags []string, gens [][]byte) (string, bool) {
	buf := make([]byte, 0, 64+len(scope)+len(sql)+16*len(args))
	buf = append(buf, keyVersion)
	buf = appendStr(buf, scope)
	buf = appendStr(buf, sql)
	buf = binary.AppendUvarint(buf, uint64(len(args)))
	for _, a := range args {
		var ok bool
		if buf, ok = appendArg(buf, a, 0); !ok {
			return "", false
		}
	}
	buf = binary.AppendUvarint(buf, uint64(len(tags)))
	for i, t := range tags {
		buf = appendStr(buf, t)
		buf = appendStr(buf, string(gens[i]))
	}
	sum := sha256.Sum256(buf)
	return prefix + "q:" + hex.EncodeToString(sum[:]), true
}

// tagKey is where a tag's generation lives.
func tagKey(prefix, tag string) string { return prefix + "t:" + tag }
