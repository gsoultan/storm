// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package caduceusdrv

import (
	"fmt"
	"time"

	"github.com/gsoultan/caduceus/codec"
	"github.com/gsoultan/storm/v2/runtime"
)

// Parameters.
//
// storm's value types carry their own binary encoders (runtime.EncodeNumeric,
// EncodeInterval, EncodeTimeOfDay, EncodeTstzRange), and runtime imports the
// standard library only, so it cannot name the caduceus interface that would
// let the driver call them. The adapter does: each such argument is wrapped in
// a type that is a codec.BinaryParam, and sent in binary: the same bytes
// storm's scanners decode, and the ones pgx's registered codecs sent.
//
// runtime.JSON is a []byte of its own name, which caduceus would hand to
// encoding/json as a value to marshal — a base64 string — so it is passed as
// the bytes it is. Generated code binds every argument through a pointer to
// its statement's arena, so each of these is wrapped as a pointer too.
//
// The argument slice is copied only when it holds one of these, so a statement
// of integers, strings and uuids is handed to the driver untouched.

// args returns in ready for caduceus.
func args(in []any) []any {
	for i, a := range in {
		if w, ok := wrap(a); ok {
			out := make([]any, len(in))
			copy(out, in[:i])
			out[i] = w
			for j := i + 1; j < len(in); j++ {
				if w, ok := wrap(in[j]); ok {
					out[j] = w
				} else {
					out[j] = in[j]
				}
			}
			return out
		}
	}
	return in
}

// wrap returns the BinaryParam for one of storm's value types, and false for
// anything caduceus encodes itself.
func wrap(a any) (any, bool) {
	switch v := a.(type) {
	case runtime.Decimal:
		return decimalParam(v), true
	case *runtime.Decimal:
		return nullable(v, func(d runtime.Decimal) any { return decimalParam(d) }), true
	case []runtime.Decimal:
		return decimalArray(v), true
	case *[]runtime.Decimal:
		if v == nil {
			return nil, true
		}
		return decimalArray(*v), true
	case runtime.Interval:
		return intervalParam(v), true
	case *runtime.Interval:
		return nullable(v, func(d runtime.Interval) any { return intervalParam(d) }), true
	case runtime.TimeOfDay:
		return timeOfDayParam(v), true
	case *runtime.TimeOfDay:
		return nullable(v, func(d runtime.TimeOfDay) any { return timeOfDayParam(d) }), true
	case runtime.TstzRange:
		return tstzRangeParam(v), true
	case *runtime.TstzRange:
		return nullable(v, func(d runtime.TstzRange) any { return tstzRangeParam(d) }), true
	case runtime.JSON:
		if v == nil {
			return nil, true
		}
		return []byte(v), true
	case *runtime.JSON:
		if v == nil || *v == nil {
			return nil, true
		}
		return []byte(*v), true
	}
	return nil, false
}

// decimalArray is a nil slice as NULL, which a text COPY could not otherwise
// tell from an empty array.
func decimalArray(v []runtime.Decimal) any {
	if v == nil {
		return nil
	}
	return decimalArrayParam(v)
}

// nullable is a pointer's value wrapped, or nil, which is NULL.
func nullable[T any](p *T, as func(T) any) any {
	if p == nil {
		return nil
	}
	return as(*p)
}

// checkOID refuses a value for a parameter of another type: the server would
// read the bytes with that type's receive function, so binary sent to the
// wrong type is either refused or, worse, read as something else.
func checkOID(name string, want, got uint32) error {
	if got == want {
		return nil
	}
	return fmt.Errorf("caduceusdrv: a %s cannot be sent for a parameter of type oid %d", name, got)
}

type decimalParam runtime.Decimal

func (p decimalParam) AppendBinaryParam(dst []byte, oid uint32) ([]byte, bool, error) {
	if err := checkOID("storm.Decimal", codec.Numeric, oid); err != nil {
		return dst, false, err
	}
	return runtime.EncodeNumeric(runtime.Decimal(p), dst), false, nil
}

type decimalArrayParam []runtime.Decimal

func (p decimalArrayParam) AppendBinaryParam(dst []byte, oid uint32) ([]byte, bool, error) {
	if p == nil {
		return dst, true, nil
	}
	if err := checkOID("[]storm.Decimal", 1231, oid); err != nil {
		return dst, false, err
	}
	return runtime.EncodeArray([]runtime.Decimal(p), codec.Numeric, dst, runtime.EncodeNumeric), false, nil
}

type intervalParam runtime.Interval

func (p intervalParam) AppendBinaryParam(dst []byte, oid uint32) ([]byte, bool, error) {
	if err := checkOID("storm.Interval", codec.Interval, oid); err != nil {
		return dst, false, err
	}
	return runtime.EncodeInterval(runtime.Interval(p), dst), false, nil
}

type timeOfDayParam runtime.TimeOfDay

func (p timeOfDayParam) AppendBinaryParam(dst []byte, oid uint32) ([]byte, bool, error) {
	if err := checkOID("storm.TimeOfDay", codec.Time, oid); err != nil {
		return dst, false, err
	}
	return runtime.EncodeTimeOfDay(runtime.TimeOfDay(p), dst), false, nil
}

type tstzRangeParam runtime.TstzRange

func (p tstzRangeParam) AppendBinaryParam(dst []byte, oid uint32) ([]byte, bool, error) {
	if err := checkOID("storm.TstzRange", 3910, oid); err != nil {
		return dst, false, err
	}
	return runtime.EncodeTstzRange(runtime.TstzRange(p), dst), false, nil
}

// MarshalText is the range's literal, such as ["2026-01-01 09:00:00Z",), for
// a COPY that another column has turned to text: caduceus cannot read a
// tstzrange's binary form back as text, so the range writes its own.
func (p tstzRangeParam) MarshalText() ([]byte, error) {
	r := runtime.TstzRange(p)
	if r.Empty {
		return []byte("empty"), nil
	}
	b := make([]byte, 0, 72)
	if r.LowerInc && !r.LowerInf {
		b = append(b, '[')
	} else {
		b = append(b, '(')
	}
	if !r.LowerInf {
		b = appendBound(b, r.Lower)
	}
	b = append(b, ',')
	if !r.UpperInf {
		b = appendBound(b, r.Upper)
	}
	if r.UpperInc && !r.UpperInf {
		b = append(b, ']')
	} else {
		b = append(b, ')')
	}
	return b, nil
}

// appendBound writes one bound to the nanosecond, with its offset, which
// PostgreSQL rounds to the microsecond it keeps.
func appendBound(b []byte, t time.Time) []byte {
	b = append(b, '"')
	b = t.AppendFormat(b, "2006-01-02 15:04:05.999999999Z07:00")
	return append(b, '"')
}

// MarshalText is the array's literal, such as {1.5,-0.007}, for a COPY that
// another column has turned to text.
func (p decimalArrayParam) MarshalText() ([]byte, error) {
	b := []byte{'{'}
	for i, d := range p {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, d.String()...)
	}
	return append(b, '}'), nil
}
