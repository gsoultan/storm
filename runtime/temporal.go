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
		return Interval{}, errors.New("raorm: interval wire value is too short")
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
