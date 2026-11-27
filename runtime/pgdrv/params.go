// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgdrv

import (
	"fmt"

	"github.com/gsoultan/caduceus/codec"
	"github.com/gsoultan/storm/runtime"
)

// Parameters.
//
// storm's value types carry their own binary encoders (runtime.EncodeNumeric,
// EncodeInterval, EncodeTimeOfDay, EncodeTstzRange), and runtime imports the
// standard library only, so it cannot name the caduceus interface that would
// let the driver call them. The adapter does: each such argument is wrapped in
// a type that is a codec.BinaryParam, and sent in binary exactly as pgx's
// registered codecs sent it.
//
// The argument slice is copied only when it holds one of these, so a statement
// of integers, strings and uuids is handed to the driver untouched.

// params returns args ready for caduceus.
func params(args []any) []any {
	for i, a := range args {
		if w, ok := wrap(a); ok {
			out := make([]any, len(args))
			copy(out, args[:i])
			out[i] = w
			for j := i + 1; j < len(args); j++ {
				if w, ok := wrap(args[j]); ok {
					out[j] = w
				} else {
					out[j] = args[j]
				}
			}
			return out
		}
	}
	return args
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
		return decimalArrayParam(v), true
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
	}
	return nil, false
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
	return fmt.Errorf("pgdrv: a %s cannot be sent for a parameter of type oid %d", name, got)
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
