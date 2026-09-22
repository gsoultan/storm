package valdec

// The canonical decoder names, so this family needs no rename map.
//
// A generated package calls `valdec.Int8`, `valdec.Timestamptz` and the rest by
// the same names runtime's own family uses — only the ARGUMENT differs, and
// that is the whole of what the second row shape changes. The SQL Server and
// MySQL families rename where their bytes genuinely mean something else
// (`DateTimeOffset` is not `Timestamptz`); here nothing does, so nothing is
// renamed and the map stays empty.

import (
	"fmt"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// UUID reads a 16-byte key.
//
// Oracle's is a RAW(16) and arrives as []byte. It is COPIED into the array,
// which is also why codegen cannot emit the byte families' `copy(r.F[:], rv[i])`
// for this shape — `rv[i]` is an `any`, and copy needs a slice.
func UUID(v any) [16]byte {
	var out [16]byte
	switch x := v.(type) {
	case []byte:
		copy(out[:], x)
	case string:
		copy(out[:], x)
	}
	return out
}

// Timestamptz reads an instant WITH its offset.
//
// Fallible here where runtime's is not, for the reason MySQL's temporals are:
// this reads whatever the driver decided to hand over, and a driver that
// yields text for a temporal can hand over text that is not one. Reporting it
// beats returning a plausible zero.
func Timestamptz(v any) (time.Time, error) { return Time(v) }

// Date reads a date. Oracle's DATE carries a time too — see compile/oraddl's
// note — so this is the same decoder under a different name rather than a
// truncation storm would be inventing.
func Date(v any) (time.Time, error) { return Time(v) }

// TimeOfDay reads a clock reading. Refused as a COLUMN type by compile/oraddl
// — Oracle has no time-of-day type — so this exists for the families that do
// and for a raw query that computes one.
func TimeOfDay(v any) (runtime.TimeOfDay, error) {
	t, err := Time(v)
	if err != nil {
		return 0, err
	}
	tod, ok := runtime.NewTimeOfDay(t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/1000)
	if !ok {
		return 0, fmt.Errorf("valdec: %s is not a time of day", t.Format("15:04:05.999999"))
	}
	return tod, nil
}

// NumericErr reads an exact number. See Decimal: through its decimal string,
// never through a float64.
func NumericErr(v any) (runtime.Decimal, error) { return Decimal(v) }

// Nullable wraps a decoder for a column that may be NULL.
func Nullable[T any](v any, dec func(any) T) runtime.Null[T] {
	if v == nil {
		return runtime.Null[T]{}
	}
	return runtime.Null[T]{V: dec(v), Valid: true}
}

// NullText is the string case. No slab, unlike every other family's: the
// driver already allocated the string.
func NullText(v any, _ *runtime.Slab) runtime.Null[string] {
	if v == nil {
		return runtime.Null[string]{}
	}
	return runtime.Null[string]{V: Str(v), Valid: true}
}

// NullNumeric, NullTimestamptz, NullDate and NullTimeOfDay pair the NULL test
// with the error, which is the convention every family follows for a decoder
// that can fail.
func NullNumeric(v any) (runtime.Null[runtime.Decimal], error) {
	if v == nil {
		return runtime.Null[runtime.Decimal]{}, nil
	}
	d, err := Decimal(v)
	if err != nil {
		return runtime.Null[runtime.Decimal]{}, err
	}
	return runtime.Null[runtime.Decimal]{V: d, Valid: true}, nil
}

func NullTimestamptz(v any) (runtime.Null[time.Time], error) { return nullTime(v) }
func NullDate(v any) (runtime.Null[time.Time], error)        { return nullTime(v) }

func nullTime(v any) (runtime.Null[time.Time], error) {
	if v == nil {
		return runtime.Null[time.Time]{}, nil
	}
	t, err := Time(v)
	if err != nil {
		return runtime.Null[time.Time]{}, err
	}
	return runtime.Null[time.Time]{V: t, Valid: true}, nil
}

func NullTimeOfDay(v any) (runtime.Null[runtime.TimeOfDay], error) {
	if v == nil {
		return runtime.Null[runtime.TimeOfDay]{}, nil
	}
	t, err := TimeOfDay(v)
	if err != nil {
		return runtime.Null[runtime.TimeOfDay]{}, err
	}
	return runtime.Null[runtime.TimeOfDay]{V: t, Valid: true}, nil
}

// JSONB reads a document. No slab: the driver's []byte is copied, because it
// is free to reuse the buffer on the next row.
func JSONB(v any, _ *runtime.Slab) []byte { return Bytes(v) }

// NullJSONB is the nullable document.
func NullJSONB(v any, _ *runtime.Slab) runtime.Null[runtime.JSON] {
	if v == nil {
		return runtime.Null[runtime.JSON]{}
	}
	return runtime.Null[runtime.JSON]{V: runtime.JSON(Bytes(v)), Valid: true}
}
