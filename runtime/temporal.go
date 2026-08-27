package runtime

import (
	"encoding/binary"
	"errors"
	"strconv"
	"time"
)

// date and interval decoding.

// pgEpoch is 2000-01-01, the zero point of PostgreSQL's binary date and
// timestamp formats.
var pgEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Date decodes a date column: an int32 of days since 2000-01-01, rendered as
// midnight UTC.
//
// UTC midnight is a CONVENTION, stated rather than implied: a SQL date has no
// time zone, and Go has no calendar-date type, so some instant has to stand in
// for the day. Midnight UTC is the one every driver picks, and comparing two
// decoded dates works because both sides picked it.
func Date(b []byte) time.Time {
	if len(b) < 4 {
		return time.Time{}
	}
	days := int32(binary.BigEndian.Uint32(b))
	return pgEpoch.AddDate(0, 0, int(days))
}

// Interval is a PostgreSQL interval, exactly as the wire carries it: months,
// days and microseconds are SEPARATE, and must stay separate.
//
// time.Duration cannot hold this. A month has no fixed length, and a day is
// not always 24 hours across a DST boundary — Postgres keeps the three fields
// apart precisely so `+ interval '1 day'` lands at the same wall-clock time
// tomorrow rather than 24 hours later. Collapsing them at decode time would
// bake in an error the database was careful not to make.
type Interval struct {
	Months int32
	Days   int32
	Micros int64
}

// Duration flattens the interval, when that is a fact rather than a guess: ok
// is false if any months are present, because a month has no length to flatten
// to. Days convert at 24h, which is the usual convention and wrong across a
// DST boundary — a caller who cares about that keeps the fields apart.
func (iv Interval) Duration() (time.Duration, bool) {
	if iv.Months != 0 {
		return 0, false
	}
	return time.Duration(iv.Days)*24*time.Hour + time.Duration(iv.Micros)*time.Microsecond, true
}

// String renders the interval in PostgreSQL's own style.
func (iv Interval) String() string {
	out := ""
	if iv.Months != 0 {
		out += strconv.Itoa(int(iv.Months)) + " mons"
	}
	if iv.Days != 0 {
		if out != "" {
			out += " "
		}
		out += strconv.Itoa(int(iv.Days)) + " days"
	}
	if iv.Micros != 0 || out == "" {
		if out != "" {
			out += " "
		}
		d := time.Duration(iv.Micros) * time.Microsecond
		out += d.String()
	}
	return out
}

// IntervalErr decodes an interval column: micros, days, months — 16 bytes.
func IntervalErr(b []byte) (Interval, error) {
	if b == nil {
		return Interval{}, nil
	}
	if len(b) < 16 {
		return Interval{}, errors.New("storm: interval wire value is too short")
	}
	return Interval{
		Micros: int64(binary.BigEndian.Uint64(b[0:8])),
		Days:   int32(binary.BigEndian.Uint32(b[8:12])),
		Months: int32(binary.BigEndian.Uint32(b[12:16])),
	}, nil
}

// NullInterval decodes a nullable interval column.
func NullInterval(b []byte) (Null[Interval], error) {
	if b == nil {
		return Null[Interval]{}, nil
	}
	iv, err := IntervalErr(b)
	if err != nil {
		return Null[Interval]{}, err
	}
	return Null[Interval]{V: iv, Valid: true}, nil
}

// EncodeInterval writes the wire format back, for binding parameters.
func EncodeInterval(iv Interval, buf []byte) []byte {
	buf = binary.BigEndian.AppendUint64(buf, uint64(iv.Micros))
	buf = binary.BigEndian.AppendUint32(buf, uint32(iv.Days))
	return binary.BigEndian.AppendUint32(buf, uint32(iv.Months))
}

// TimeOfDay is a PostgreSQL `time` (without time zone): microseconds since
// midnight, with no date and no zone.
//
// It is its own type rather than a time.Time for the same reason Interval is
// not a Duration. A time.Time is an instant — a point on a calendar in a zone
// — and a SQL `time` is none of those things. Decoding one into the other
// forces a date to be invented (drivers usually pick 0000-01-01 or today),
// and then two values that the database says are equal compare unequal, or
// arithmetic silently crosses a DST boundary that does not exist for a column
// with no zone. The int64 is exact, comparable with ==, and ordered by <,
// which is what a query builder needs.
//
// Postgres allows 24:00:00 exactly, so the range is [0, 86400000000]
// inclusive — one microsecond past 23:59:59.999999 is a legal value, not an
// overflow.
type TimeOfDay int64

// MaxTimeOfDay is 24:00:00, which PostgreSQL accepts as a `time`.
const MaxTimeOfDay TimeOfDay = 24 * 60 * 60 * 1_000_000

// NewTimeOfDay builds a time of day from its parts. Out-of-range parts are
// NOT normalised: 25:00 is a caller's mistake, and returning ok=false says so
// rather than silently meaning 01:00 the next day, which is a different fact.
func NewTimeOfDay(hour, min, sec, micro int) (TimeOfDay, bool) {
	if hour < 0 || min < 0 || sec < 0 || micro < 0 ||
		min > 59 || sec > 59 || micro > 999_999 {
		return 0, false
	}
	t := TimeOfDay(hour)*3_600_000_000 + TimeOfDay(min)*60_000_000 +
		TimeOfDay(sec)*1_000_000 + TimeOfDay(micro)
	if t < 0 || t > MaxTimeOfDay {
		return 0, false
	}
	return t, true
}

// Parts splits the value back out.
func (t TimeOfDay) Parts() (hour, min, sec, micro int) {
	v := int64(t)
	return int(v / 3_600_000_000), int(v / 60_000_000 % 60),
		int(v / 1_000_000 % 60), int(v % 1_000_000)
}

// Duration is the time since midnight. Unlike Interval.Duration this is
// always exact: a time of day has no months and no days to guess at.
func (t TimeOfDay) Duration() time.Duration {
	return time.Duration(t) * time.Microsecond
}

// String renders it the way PostgreSQL does: HH:MM:SS, with fractional
// seconds only when there are any.
func (t TimeOfDay) String() string {
	h, m, s, us := t.Parts()
	out := twoDigit(h) + ":" + twoDigit(m) + ":" + twoDigit(s)
	if us == 0 {
		return out
	}
	frac := strconv.Itoa(us + 1_000_000)[1:] // zero-pad to six
	for len(frac) > 1 && frac[len(frac)-1] == '0' {
		frac = frac[:len(frac)-1]
	}
	return out + "." + frac
}

func twoDigit(v int) string {
	if v < 10 && v >= 0 {
		return "0" + strconv.Itoa(v)
	}
	return strconv.Itoa(v)
}

// TimeOfDayErr decodes a `time` column: an int64 of microseconds since
// midnight, 8 bytes.
func TimeOfDayErr(b []byte) (TimeOfDay, error) {
	if b == nil {
		return 0, nil
	}
	if len(b) < 8 {
		return 0, errors.New("storm: time wire value is too short")
	}
	return TimeOfDay(int64(binary.BigEndian.Uint64(b))), nil
}

// NullTimeOfDay decodes a nullable `time` column.
func NullTimeOfDay(b []byte) (Null[TimeOfDay], error) {
	if b == nil {
		return Null[TimeOfDay]{}, nil
	}
	t, err := TimeOfDayErr(b)
	if err != nil {
		return Null[TimeOfDay]{}, err
	}
	return Null[TimeOfDay]{V: t, Valid: true}, nil
}

// EncodeTimeOfDay writes the wire format back, for binding parameters.
func EncodeTimeOfDay(t TimeOfDay, buf []byte) []byte {
	return binary.BigEndian.AppendUint64(buf, uint64(t))
}
