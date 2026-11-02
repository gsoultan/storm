// Package bsondec decodes the BSON values runtime/mgdrv hands a generated
// scanner — the fifth decoder family, after PostgreSQL's, MySQL's, SQL
// Server's and the database/sql value family (ADR-0007).
//
// A value arrives as its BSON type byte followed by the value's own bytes,
// exactly as they sat in the document: nothing is decoded before the scanner
// asks. A nil slice is NULL, which on MongoDB means the field was null OR
// absent — a document store has no column a row can lack, so absence is how a
// NULL is usually stored.
//
// Every decoder is FALLIBLE, where most of PostgreSQL's are not. A column
// there has one wire type, fixed by its declaration; a field here has whatever
// type the document that holds it was written with. A validator (compile/mgddl)
// makes a mismatch rare, but a collection written before it existed can hold
// anything, and a plausible zero would be worse than an error.
//
// No reflect, as everywhere under runtime/.
package bsondec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// BSON type bytes this family reads.
const (
	TDouble   = 0x01
	TString   = 0x02
	TDocument = 0x03
	TArray    = 0x04
	TBinary   = 0x05
	TBool     = 0x08
	TDateTime = 0x09
	TNull     = 0x0A
	TInt32    = 0x10
	TInt64    = 0x12
	TDecimal  = 0x13
)

func shape(want string, b []byte) error {
	t := byte(0)
	if len(b) > 0 {
		t = b[0]
	}
	return fmt.Errorf("%w: want %s, the document holds BSON type 0x%02x", runtime.ErrShape, want, t)
}

// Bool decodes a BSON boolean.
func Bool(b []byte) (bool, error) {
	if len(b) != 2 || b[0] != TBool {
		return false, shape("bool", b)
	}
	return b[1] != 0, nil
}

// Int8 decodes a 64-bit integer, accepting a 32-bit one: a shell or another
// driver writes a small number as int32, and widening it is exact.
func Int8(b []byte) (int64, error) {
	switch {
	case len(b) == 9 && b[0] == TInt64:
		return int64(binary.LittleEndian.Uint64(b[1:])), nil
	case len(b) == 5 && b[0] == TInt32:
		return int64(int32(binary.LittleEndian.Uint32(b[1:]))), nil
	}
	return 0, shape("an integer", b)
}

// Int4 decodes a 32-bit integer, accepting a 64-bit one that fits.
func Int4(b []byte) (int32, error) {
	v, err := Int8(b)
	if err != nil {
		return 0, err
	}
	if v < math.MinInt32 || v > math.MaxInt32 {
		return 0, fmt.Errorf("%w: %d does not fit an int32", runtime.ErrShape, v)
	}
	return int32(v), nil
}

// Int2 decodes a 16-bit integer, from either integer width when it fits.
func Int2(b []byte) (int16, error) {
	v, err := Int8(b)
	if err != nil {
		return 0, err
	}
	if v < math.MinInt16 || v > math.MaxInt16 {
		return 0, fmt.Errorf("%w: %d does not fit an int16", runtime.ErrShape, v)
	}
	return int16(v), nil
}

// Float8 decodes a double, accepting an integer.
func Float8(b []byte) (float64, error) {
	if len(b) == 9 && b[0] == TDouble {
		return math.Float64frombits(binary.LittleEndian.Uint64(b[1:])), nil
	}
	v, err := Int8(b)
	if err != nil {
		return 0, shape("a number", b)
	}
	return float64(v), nil
}

// Float4 decodes a double into a float32.
func Float4(b []byte) (float32, error) {
	v, err := Float8(b)
	return float32(v), err
}

// Str decodes a BSON string. It allocates: the bytes belong to a reply buffer
// the driver reuses, so the string must own its own.
func Str(b []byte) (string, error) {
	if len(b) < 6 || b[0] != TString {
		return "", shape("string", b)
	}
	n := int(int32(binary.LittleEndian.Uint32(b[1:])))
	if n < 1 || 5+n != len(b) || b[len(b)-1] != 0 {
		return "", fmt.Errorf("%w: malformed BSON string", runtime.ErrShape)
	}
	return string(b[5 : 4+n]), nil
}

// binaryOf returns a binary value's subtype and payload.
func binaryOf(b []byte) (byte, []byte, error) {
	if len(b) < 6 || b[0] != TBinary {
		return 0, nil, shape("binary", b)
	}
	n := int(int32(binary.LittleEndian.Uint32(b[1:])))
	if n < 0 || 6+n != len(b) {
		return 0, nil, fmt.Errorf("%w: malformed BSON binary", runtime.ErrShape)
	}
	return b[5], b[6:], nil
}

// Bytes decodes binary data, copied out of the driver's buffer.
func Bytes(b []byte) ([]byte, error) {
	if b == nil {
		return nil, nil
	}
	_, p, err := binaryOf(b)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), p...), nil
}

// UUID decodes a binary of subtype 4 (RFC 4122), or 3 (the legacy subtype some
// drivers still write), holding sixteen bytes.
func UUID(b []byte) ([16]byte, error) {
	var u [16]byte
	sub, p, err := binaryOf(b)
	if err != nil {
		return u, err
	}
	if (sub != 4 && sub != 3) || len(p) != 16 {
		return u, fmt.Errorf("%w: binary subtype %d of %d bytes is not a uuid", runtime.ErrShape, sub, len(p))
	}
	copy(u[:], p)
	return u, nil
}

// Time decodes a BSON datetime: milliseconds since the epoch, UTC. It holds
// milliseconds where PostgreSQL holds microseconds, so a timestamp written and
// read back is truncated to the millisecond — docs/DIALECTS.md says so.
func Time(b []byte) (time.Time, error) {
	if len(b) != 9 || b[0] != TDateTime {
		return time.Time{}, shape("datetime", b)
	}
	return time.UnixMilli(int64(binary.LittleEndian.Uint64(b[1:]))).UTC(), nil
}

// Date decodes a datetime stored as midnight UTC, as a date.
func Date(b []byte) (time.Time, error) {
	t, err := Time(b)
	if err != nil {
		return t, err
	}
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
}

// ErrDecimalSpecial is a Decimal128 infinity or NaN, which a fixed-point
// Decimal cannot carry.
var ErrDecimalSpecial = errors.New("storm: decimal128 is NaN or infinite, which has no Decimal representation")

// Decimal decodes a Decimal128 into a runtime.Decimal, refusing a value past
// its 18 significant digits rather than rounding it.
func Decimal(b []byte) (runtime.Decimal, error) {
	if len(b) != 17 || b[0] != TDecimal {
		return runtime.Decimal{}, shape("decimal128", b)
	}
	lo := binary.LittleEndian.Uint64(b[1:])
	hi := binary.LittleEndian.Uint64(b[9:])
	neg := hi>>63 == 1
	if hi>>61&3 == 3 {
		// The second form: infinity and NaN, or a coefficient above 2^113,
		// which the specification makes non-canonical — read as zero there
		// and never written by a server.
		if hi>>59&0xf == 0xf || hi>>58&0x1f == 0x1e {
			return runtime.Decimal{}, ErrDecimalSpecial
		}
		return runtime.Decimal{}, nil
	}
	exp := int32(hi>>49&0x3fff) - 6176
	if hi&(1<<49-1) != 0 || lo > math.MaxInt64 {
		return runtime.Decimal{}, runtime.ErrDecimalRange
	}
	u := int64(lo)
	for ; exp > 0; exp-- {
		if u > math.MaxInt64/10 {
			return runtime.Decimal{}, runtime.ErrDecimalRange
		}
		u *= 10
	}
	if neg {
		u = -u
	}
	return runtime.Decimal{Unscaled: u, Scale: -exp}, nil
}

// Nullable decodes a value that may be NULL — nil — with dec.
func Nullable[T any](b []byte, dec func([]byte) (T, error)) (runtime.Null[T], error) {
	if b == nil {
		return runtime.Null[T]{}, nil
	}
	v, err := dec(b)
	if err != nil {
		return runtime.Null[T]{}, err
	}
	return runtime.Null[T]{V: v, Valid: true}, nil
}
