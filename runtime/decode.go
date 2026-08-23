package runtime

import (
	"encoding/binary"
	"math"
	"time"
)

// Wire decoders. Generated scanners call these directly: no reflect, no
// driver.Value, no `any`. Text columns route through a Slab so a result costs
// a handful of allocations rather than one per string per row.

// PGEpoch is Postgres' timestamp origin.
var PGEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

func Bool(b []byte) bool  { return len(b) > 0 && b[0] != 0 }
func Int2(b []byte) int16 { return int16(binary.BigEndian.Uint16(b)) }
func Int4(b []byte) int32 { return int32(binary.BigEndian.Uint32(b)) }
func Int8(b []byte) int64 { return int64(binary.BigEndian.Uint64(b)) }
func Float4(b []byte) float32 {
	return math.Float32frombits(binary.BigEndian.Uint32(b))
}
func Float8(b []byte) float64 {
	return math.Float64frombits(binary.BigEndian.Uint64(b))
}

func UUID(b []byte) [16]byte {
	var u [16]byte
	copy(u[:], b)
	return u
}

func Timestamptz(b []byte) time.Time {
	return PGEpoch.Add(time.Duration(int64(binary.BigEndian.Uint64(b))) * time.Microsecond)
}

// Bytes copies, because the wire buffer is reused on the next row.
func Bytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// Null[T] mirrors the public raorm.Null so generated code needs no import
// cycle back to the root package.
type Null[T any] struct {
	V     T
	Valid bool
}

func (n Null[T]) Get() (T, bool) { return n.V, n.Valid }

// Arg is what a driver binds for this column: the value, or nil for NULL.
//
// Writes build a []any regardless, so the box here is not an allocation the
// write path could have avoided — it is the one the driver interface requires.
func (n Null[T]) Arg() any {
	if !n.Valid {
		return nil
	}
	return n.V
}

// Nullable wraps a decoder for a column that may be NULL.
func Nullable[T any](b []byte, dec func([]byte) T) Null[T] {
	if b == nil {
		return Null[T]{}
	}
	return Null[T]{V: dec(b), Valid: true}
}

// NullText is the string case, which needs the arena.
func NullText(b []byte, s *Slab) Null[string] {
	if b == nil {
		return Null[string]{}
	}
	return Null[string]{V: s.Str(b), Valid: true}
}
