package runtime

import (
	"encoding/binary"
	"encoding/json"
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

// Null[T] mirrors the public storm.Null so generated code needs no import
// cycle back to the root package.
type Null[T any] struct {
	V     T
	Valid bool
}

func (n Null[T]) Get() (T, bool) { return n.V, n.Valid }

// Ptr is what a bulk load binds: a pointer to the value, or a nil *T for NULL.
//
// Pointer, not value, because boxing a value into an `any` allocates and boxing
// a pointer does not. On a COPY of a thousand rows that is the difference
// between one allocation per column per row and none — the same trick that took
// the read path's binder to zero.
func (n *Null[T]) Ptr() *T {
	if !n.Valid {
		return nil
	}
	return &n.V
}

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

// Numeric decodes a numeric column. An out-of-range or NaN value is an error
// rather than a wrong number, so the scanner records it rather than storing a
// plausible zero — see Slab-free decoding in generated scan().
func Numeric(b []byte) Decimal {
	d, err := DecodeNumeric(b)
	if err != nil {
		// Reaching here means the column holds a value a Decimal cannot carry.
		// The generated scanner checks DecimalErr immediately after, so this
		// zero never escapes as a result.
		return Decimal{}
	}
	return d
}

// NumericErr is Numeric with the error, for the paths that can report one.
func NumericErr(b []byte) (Decimal, error) {
	if b == nil {
		return Decimal{}, nil
	}
	return DecodeNumeric(b)
}

// JSONB copies a jsonb column's bytes into the arena and strips PostgreSQL's
// version prefix.
//
// The binary format is one version byte (currently 1) followed by the JSON
// text. Handing the caller the prefix would make every unmarshal fail, and
// stripping it in the caller would make every caller know the wire format.
func JSONB(b []byte, s *Slab) []byte {
	if len(b) == 0 {
		return nil
	}
	if b[0] == 1 {
		b = b[1:]
	}
	return s.Bytes(b)
}

// JSON is a jsonb column's raw bytes. It is a named type so a generated Row
// field says what it is, and so Unmarshal can hang off it.
type JSON []byte

// Unmarshal decodes into v. It is a method rather than a free function so the
// call site reads as what it is — the caller supplying the shape the generator
// could not know.
func (j JSON) Unmarshal(v any) error {
	if len(j) == 0 {
		return nil
	}
	return json.Unmarshal(j, v)
}

// String renders the raw JSON, for logs and errors.
func (j JSON) String() string { return string(j) }

// NullJSON decodes a nullable jsonb column.
func NullJSON(b []byte, s *Slab) Null[JSON] {
	if b == nil {
		return Null[JSON]{}
	}
	return Null[JSON]{V: JSON(JSONB(b, s)), Valid: true}
}

// NullNumeric decodes a nullable numeric column.
func NullNumeric(b []byte) (Null[Decimal], error) {
	if b == nil {
		return Null[Decimal]{}, nil
	}
	d, err := DecodeNumeric(b)
	if err != nil {
		return Null[Decimal]{}, err
	}
	return Null[Decimal]{V: d, Valid: true}, nil
}
