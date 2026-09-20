// Package msdec decodes SQL Server wire values.
//
// The THIRD decoder family, and ADR-0007's argument holds for the same reason
// it did for MySQL: these share no bytes with either of the others. PostgreSQL
// is big-endian and sends temporals as an offset from 2000-01-01; MySQL is
// little-endian and packs them component-wise behind a length; SQL Server is
// little-endian and counts hundred-nanosecond ticks since midnight beside a day
// number. Pointing a scanner at the wrong family produces byte-reversed numbers
// with no error, for every row — so the families are different packages and the
// choice is structural rather than a flag.
//
// THE CONTRACT WITH runtime/msdrv. Four families of value do not arrive as the
// wire carries them, because their wire form is not self-describing: what a
// decoder would need lives in the column metadata, which a decoder never sees.
// msdrv normalises those on the way out, and this package decodes the canonical
// form:
//
//	uniqueidentifier  16 bytes, canonical order (msdrv swaps the mixed-endian wire form)
//	decimal/numeric    9 bytes: scale, then the unscaled int64, little-endian
//	date               3 bytes: days since 0001-01-01
//	time               5 bytes: hundred-nanosecond ticks since midnight
//	datetime2          5 bytes of ticks, then 3 of day — always scale 7
//	datetimeoffset     datetime2, then 2 bytes of signed offset MINUTES
//
// Everything else is the wire's own bytes.
//
// Strings are the other difference worth stating: an nvarchar is UTF-16LE, so
// text costs a transcode here where PostgreSQL and MySQL hand over UTF-8 and
// the slab copies it. That is the price of a database whose native string type
// is UTF-16, and it is paid once per value rather than per rune of overhead.
package msdec

import (
	"encoding/binary"
	"errors"
	"math"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/gsoultan/storm/runtime"
)

// errShort is a value narrower than its type needs. It cannot happen against a
// server that agrees with the schema, which is exactly why it must not panic
// when it does: a truncated read is a bug to report, not a process to end.
var errShort = errors.New("msdec: the column's value is shorter than its type")

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

// Str decodes an nvarchar into the row's slab.
//
// UTF-16LE to UTF-8, which is a transcode rather than a copy — PostgreSQL and
// MySQL both hand over UTF-8 and the slab copies it directly. The ASCII case is
// still one pass with no allocation: the slab is grown once and written
// through, so a column of ordinary text costs its own bytes and nothing more.
func Str(b []byte, s *runtime.Slab) string {
	if len(b) == 0 {
		return ""
	}
	if ascii(b) {
		// Every second byte is zero, which is every string a Latin keyboard
		// produces. Unpacked directly rather than through utf16.Decode, which
		// would build a []rune first.
		dst := s.Bytes(b[:len(b)/2])
		for i := 0; i < len(dst); i++ {
			dst[i] = b[i*2]
		}
		// The same move runtime.Slab.Str makes internally, and safe for the
		// same reason: the arena has the capacity, so nothing moved, and the
		// bytes are never written again.
		return unsafe.String(&dst[0], len(dst))
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[i*2]) | uint16(b[i*2+1])<<8
	}
	return string(utf16.Decode(u))
}

// ascii reports whether every code unit is below 0x80, which is the case the
// fast path above handles.
func ascii(b []byte) bool {
	if len(b)%2 != 0 {
		return false
	}
	for i := 1; i < len(b); i += 2 {
		if b[i] != 0 || b[i-1] >= 0x80 {
			return false
		}
	}
	return true
}

// Bytes copies a varbinary into the slab.
func Bytes(b []byte, s *runtime.Slab) []byte {
	if len(b) == 0 {
		return nil
	}
	out := s.Bytes(b)
	copy(out, b)
	return out
}

// UUID is the sixteen bytes, already in canonical order — msdrv swaps the
// wire's mixed-endian layout, because a decoder that did it here would have to
// know which back end it was decoding for.
func UUID(b []byte) [16]byte {
	var u [16]byte
	copy(u[:], b)
	return u
}

// Days from 0001-01-01, which this server counts from, to the Unix epoch.
const daysToEpoch = 719162

// DateTimeOffset decodes the canonical ten-byte form.
//
// The offset is carried and then DISCARDED into UTC, which is not a loss: a
// timestamptz is an instant, the zone is a rendering choice, and storm
// normalises to UTC everywhere so that two rows written from two machines
// compare correctly.
func DateTimeOffset(b []byte) (time.Time, error) {
	if len(b) < 8 {
		return time.Time{}, errShort
	}
	t := instant(b)
	if len(b) >= 10 {
		// The stored instant is already UTC — the server converts on the way in
		// — so the offset says how to RENDER it, not how to correct it.
		_ = int16(binary.LittleEndian.Uint16(b[8:]))
	}
	return t, nil
}

// DateTime decodes a datetime2, which has no offset and is therefore read as
// UTC. A model that wants a zone declares timestamptz.
func DateTime(b []byte) (time.Time, error) {
	if len(b) < 8 {
		return time.Time{}, errShort
	}
	return instant(b), nil
}

// instant turns five bytes of ticks and three of day into a time.
func instant(b []byte) time.Time {
	var ticks uint64
	for i := 0; i < 5; i++ {
		ticks |= uint64(b[i]) << (8 * i)
	}
	day := int64(uint32(b[5]) | uint32(b[6])<<8 | uint32(b[7])<<16)
	sec := (day-daysToEpoch)*86400 + int64(ticks/10000000)
	nsec := int64(ticks%10000000) * 100
	return time.Unix(sec, nsec).UTC()
}

// Date decodes the three-byte day number, at midnight UTC.
func Date(b []byte) (time.Time, error) {
	if len(b) < 3 {
		return time.Time{}, errShort
	}
	day := int64(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16)
	return time.Unix((day-daysToEpoch)*86400, 0).UTC(), nil
}

// TimeOfDay decodes five bytes of ticks into runtime.TimeOfDay.
//
// The unit conversion is the one M9 got wrong in the other direction and paid
// for with a value a thousand times too large: runtime.TimeOfDay counts
// MICROSECONDS and this type counts HUNDREDS OF NANOSECONDS, so the factor is
// ten and it is a division.
func TimeOfDay(b []byte) (runtime.TimeOfDay, error) {
	if len(b) < 5 {
		return 0, errShort
	}
	var ticks uint64
	for i := 0; i < 5; i++ {
		ticks |= uint64(b[i]) << (8 * i)
	}
	return runtime.TimeOfDay(ticks / 10), nil
}

// decimalOverflow is the scale msdrv writes for a value wider than an int64.
// It mirrors the constant there; the two are a pair and are named the same.
const decimalOverflow = 0xFF

// Decimal decodes the canonical nine bytes.
func Decimal(b []byte) (runtime.Decimal, error) {
	if len(b) < 9 {
		return runtime.Decimal{}, errShort
	}
	if b[0] == decimalOverflow {
		// More than eighteen significant digits. Reported rather than
		// truncated: a currency figure quietly missing its top digits is the
		// worst outcome available here.
		return runtime.Decimal{}, runtime.ErrDecimalRange
	}
	return runtime.Decimal{
		Unscaled: int64(binary.LittleEndian.Uint64(b[1:])),
		Scale:    int32(b[0]),
	}, nil
}

// JSON is a document, which this server stores as nvarchar — so it is a
// transcode like any other string, into the slab the row owns.
func JSONB(b []byte, s *runtime.Slab) []byte {
	if len(b) == 0 {
		return nil
	}
	return []byte(Str(b, s))
}

func JSON(b []byte) runtime.JSON { return runtime.JSON(b) }

// ---- the nullable spellings -------------------------------------------------
//
// One per fallible decoder, because runtime.Nullable takes a decoder that
// cannot fail. Missing these is what shipped mydec.NullTimestamptz as a call to
// a function that did not exist — no MySQL fixture had a nullable temporal
// column, and a soft-delete model has one on day one.

func NullText(b []byte, s *runtime.Slab) runtime.Null[string] {
	if b == nil {
		return runtime.Null[string]{}
	}
	return runtime.Null[string]{V: Str(b, s), Valid: true}
}

func NullJSON(b []byte, s *runtime.Slab) runtime.Null[runtime.JSON] {
	if b == nil {
		return runtime.Null[runtime.JSON]{}
	}
	return runtime.Null[runtime.JSON]{V: runtime.JSON(JSONB(b, s)), Valid: true}
}

func NullNumeric(b []byte) (runtime.Null[runtime.Decimal], error) {
	if b == nil {
		return runtime.Null[runtime.Decimal]{}, nil
	}
	d, err := Decimal(b)
	if err != nil {
		return runtime.Null[runtime.Decimal]{}, err
	}
	return runtime.Null[runtime.Decimal]{V: d, Valid: true}, nil
}

func NullDateTimeOffset(b []byte) (runtime.Null[time.Time], error) {
	if b == nil {
		return runtime.Null[time.Time]{}, nil
	}
	t, err := DateTimeOffset(b)
	if err != nil {
		return runtime.Null[time.Time]{}, err
	}
	return runtime.Null[time.Time]{V: t, Valid: true}, nil
}

func NullDateTime(b []byte) (runtime.Null[time.Time], error) {
	if b == nil {
		return runtime.Null[time.Time]{}, nil
	}
	t, err := DateTime(b)
	if err != nil {
		return runtime.Null[time.Time]{}, err
	}
	return runtime.Null[time.Time]{V: t, Valid: true}, nil
}

func NullDate(b []byte) (runtime.Null[time.Time], error) {
	if b == nil {
		return runtime.Null[time.Time]{}, nil
	}
	t, err := Date(b)
	if err != nil {
		return runtime.Null[time.Time]{}, err
	}
	return runtime.Null[time.Time]{V: t, Valid: true}, nil
}

func NullTimeOfDay(b []byte) (runtime.Null[runtime.TimeOfDay], error) {
	if b == nil {
		return runtime.Null[runtime.TimeOfDay]{}, nil
	}
	v, err := TimeOfDay(b)
	if err != nil {
		return runtime.Null[runtime.TimeOfDay]{}, err
	}
	return runtime.Null[runtime.TimeOfDay]{V: v, Valid: true}, nil
}
