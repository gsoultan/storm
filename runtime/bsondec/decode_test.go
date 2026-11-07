// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package bsondec

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
)

func i32(t byte, v int32) []byte { return binary.LittleEndian.AppendUint32([]byte{t}, uint32(v)) }
func i64(t byte, v int64) []byte { return binary.LittleEndian.AppendUint64([]byte{t}, uint64(v)) }

func str(s string) []byte {
	b := binary.LittleEndian.AppendUint32([]byte{TString}, uint32(len(s)+1))
	return append(append(b, s...), 0)
}

func bin(sub byte, p []byte) []byte {
	b := binary.LittleEndian.AppendUint32([]byte{TBinary}, uint32(len(p)))
	return append(append(b, sub), p...)
}

func dec(lo, hi uint64) []byte {
	b := binary.LittleEndian.AppendUint64([]byte{TDecimal}, lo)
	return binary.LittleEndian.AppendUint64(b, hi)
}

func TestScalars(t *testing.T) {
	if v, err := Bool([]byte{TBool, 1}); !v || err != nil {
		t.Error("bool")
	}
	if _, err := Bool(str("x")); !errors.Is(err, runtime.ErrShape) {
		t.Error("bool from a string must be a shape error")
	}
	if v, _ := Int8(i32(TInt32, -7)); v != -7 {
		t.Error("an int32 widens")
	}
	if v, _ := Int8(i64(TInt64, 1<<40)); v != 1<<40 {
		t.Error("int64")
	}
	if _, err := Int8(nil); err == nil {
		t.Error("a missing NOT NULL field is an error, not a zero")
	}
	if v, _ := Int4(i64(TInt64, 5)); v != 5 {
		t.Error("an int64 that fits narrows")
	}
	if _, err := Int4(i64(TInt64, 1<<40)); err == nil {
		t.Error("an int64 that does not fit is refused")
	}
	if _, err := Int4(str("x")); err == nil {
		t.Error("int4 from text")
	}
	if v, _ := Int2(i32(TInt32, -300)); v != -300 {
		t.Error("int2")
	}
	if _, err := Int2(i32(TInt32, 1<<20)); err == nil {
		t.Error("int2 overflow")
	}
	if _, err := Int2(str("x")); err == nil {
		t.Error("int2 from text")
	}
	if v, _ := Float8(i64(TDouble, int64(math.Float64bits(2.5)))); v != 2.5 {
		t.Error("double")
	}
	if v, _ := Float8(i32(TInt32, 3)); v != 3 {
		t.Error("an integer reads as a double")
	}
	if _, err := Float8(str("x")); err == nil {
		t.Error("double from text")
	}
	if v, _ := Float4(i64(TDouble, int64(math.Float64bits(0.5)))); v != 0.5 {
		t.Error("float4")
	}
	if v, _ := Str(str("héllo")); v != "héllo" {
		t.Error("string")
	}
	if _, err := Str([]byte{TString, 9, 0, 0, 0, 'a', 0}); err == nil {
		t.Error("a string whose length lies is refused")
	}
	if _, err := Str(i32(TInt32, 1)); err == nil {
		t.Error("string from an int")
	}
	at := time.Date(2026, 3, 4, 5, 6, 7, 8_000_000, time.UTC)
	if v, _ := Time(i64(TDateTime, at.UnixMilli())); !v.Equal(at) {
		t.Error("datetime")
	}
	if _, err := Time(str("x")); err == nil {
		t.Error("datetime from text")
	}
	if v, _ := Date(i64(TDateTime, at.UnixMilli())); !v.Equal(time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)) {
		t.Error("date")
	}
	if _, err := Date(nil); err == nil {
		t.Error("date from nothing")
	}
}

func TestBinary(t *testing.T) {
	u := [16]byte{1, 2, 3}
	if v, err := UUID(bin(4, u[:])); v != u || err != nil {
		t.Error("uuid")
	}
	if v, _ := UUID(bin(3, u[:])); v != u {
		t.Error("the legacy subtype is a uuid too")
	}
	if _, err := UUID(bin(0, u[:])); err == nil {
		t.Error("generic binary is not a uuid")
	}
	if _, err := UUID(str("x")); err == nil {
		t.Error("uuid from text")
	}
	if _, err := UUID([]byte{TBinary, 99, 0, 0, 0, 0}); err == nil {
		t.Error("a binary whose length lies is refused")
	}
	raw := []byte{TBinary, 2, 0, 0, 0, 0, 9, 8}
	v, err := Bytes(raw)
	if err != nil || len(v) != 2 || v[0] != 9 {
		t.Fatal(v, err)
	}
	raw[6] = 0
	if v[0] != 9 {
		t.Error("Bytes must copy out of the driver's buffer")
	}
	if v, err := Bytes(nil); v != nil || err != nil {
		t.Error("NULL bytes are nil")
	}
	if _, err := Bytes(str("x")); err == nil {
		t.Error("bytes from text")
	}
}

func TestDecimal128(t *testing.T) {
	exp := func(e int) uint64 { return uint64(6176+e) << 49 }
	cases := []struct {
		b    []byte
		want runtime.Decimal
	}{
		{dec(12345, exp(-2)), runtime.Decimal{Unscaled: 12345, Scale: 2}},
		{dec(12345, 1<<63|exp(-2)), runtime.Decimal{Unscaled: -12345, Scale: 2}},
		{dec(7, exp(3)), runtime.Decimal{Unscaled: 7000}},
		{dec(0, exp(0)), runtime.Decimal{}},
	}
	for _, c := range cases {
		if got, err := Decimal(c.b); err != nil || got != c.want {
			t.Errorf("%x: %+v %v", c.b, got, err)
		}
	}
	if _, err := Decimal(dec(math.MaxUint64, exp(0))); !errors.Is(err, runtime.ErrDecimalRange) {
		t.Error("a coefficient past int64 is out of range, not wrapped")
	}
	if _, err := Decimal(dec(0, exp(0)|1)); !errors.Is(err, runtime.ErrDecimalRange) {
		t.Error("a coefficient past 64 bits is out of range")
	}
	if _, err := Decimal(dec(math.MaxInt64/2, exp(5))); !errors.Is(err, runtime.ErrDecimalRange) {
		t.Error("scaling up past int64 is out of range")
	}
	if _, err := Decimal(dec(0, 0x7c00<<48)); !errors.Is(err, ErrDecimalSpecial) {
		t.Error("NaN")
	}
	if _, err := Decimal(dec(0, 0x7800<<48)); !errors.Is(err, ErrDecimalSpecial) {
		t.Error("infinity")
	}
	if v, err := Decimal(dec(0, 0x6000<<48)); err != nil || v != (runtime.Decimal{}) {
		t.Error("a non-canonical large coefficient reads as zero, as the specification says")
	}
	if _, err := Decimal(i32(TInt32, 1)); err == nil {
		t.Error("decimal from an int")
	}
}

func TestNullable(t *testing.T) {
	if v, err := Nullable(nil, Int8); v.Valid || err != nil {
		t.Error("nil is NULL")
	}
	if v, _ := Nullable(i32(TInt32, 4), Int8); !v.Valid || v.V != 4 {
		t.Error("a value")
	}
	if _, err := Nullable(str("x"), Int8); err == nil {
		t.Error("a wrong type is still an error")
	}
}
