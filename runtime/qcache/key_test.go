// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package qcache

import (
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
)

type valuer struct {
	v   driver.Value
	err error
}

func (v valuer) Value() (driver.Value, error) { return v.v, v.err }

type loop struct{}

func (loop) Value() (driver.Value, error) { return loop{}, nil }

type marshaler struct {
	b   []byte
	err error
}

func (m marshaler) MarshalBinary() ([]byte, error) { return m.b, m.err }

// Every value here must key, and no two may share a key: a shared key is two
// different queries answered with one result.
func TestEveryArgumentTypeKeysDistinctly(t *testing.T) {
	when := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	s, i64, i32, i, b, f, tm := "s", int64(1), int32(1), 1, true, 1.0, when
	args := []any{
		nil, "", "s", []byte("s"), runtime.JSON("s"), []byte{}, runtime.JSON(`{}`),
		true, false,
		int(1), int8(1), int16(1), int32(1), int64(1), int64(-1),
		uint(1), uint8(1), uint16(1), uint32(1), uint64(1),
		float32(1), float64(1),
		when, when.In(time.FixedZone("x", 3600)), time.Second,
		runtime.Decimal{Unscaled: 1}, runtime.Decimal{Unscaled: 1, Scale: 1},
		runtime.Interval{Days: 1}, runtime.TimeOfDay(1),
		runtime.NewTstzRange(when, when.Add(time.Hour)), runtime.Since(when),
		[16]byte{1},
		[]string{"a", "b"}, []string{"ab"}, []int64{1}, []int32{1}, []int{1},
		[]bool{true, false}, []float64{1}, [][]byte{[]byte("a"), nil},
		[]time.Time{when}, [][16]byte{{2}},
		valuer{v: "via-valuer"}, marshaler{b: []byte("bin")},
	}
	seen := map[string]int{}
	for n, a := range args {
		k, ok := entryKey("p:", "", "q", []any{a}, nil, nil)
		if !ok {
			t.Fatalf("arg %d (%T) did not key", n, a)
		}
		if prev, dup := seen[k]; dup {
			t.Fatalf("arg %d (%T) keys the same as arg %d (%T)", n, a, prev, args[prev])
		}
		seen[k] = n
	}

	// A pointer keys as what it points at: the driver binds the same value.
	for _, pair := range [][2]any{{&s, s}, {&i64, i64}, {&i32, i32}, {&i, i}, {&b, b}, {&f, f}, {&tm, tm}} {
		k1, ok1 := entryKey("p:", "", "q", []any{pair[0]}, nil, nil)
		k2, _ := entryKey("p:", "", "q", []any{pair[1]}, nil, nil)
		if !ok1 || k1 != k2 {
			t.Fatalf("%T does not key as the value it points at", pair[0])
		}
	}

	// A nil pointer and a nil slice are NULL, like nil itself.
	nilKey, _ := entryKey("p:", "", "q", []any{nil}, nil, nil)
	for _, a := range []any{(*string)(nil), (*int64)(nil), (*int32)(nil), (*int)(nil), (*bool)(nil),
		(*float64)(nil), (*time.Time)(nil), []byte(nil), runtime.JSON(nil)} {
		if k, _ := entryKey("p:", "", "q", []any{a}, nil, nil); k != nilKey {
			t.Fatalf("%T(nil) is not keyed as NULL", a)
		}
	}
}

func TestArgumentsItCannotEncodeRefuseAKey(t *testing.T) {
	bad := errors.New("bad")
	broken := time.Date(1, 1, 1, 0, 0, 0, 0, time.FixedZone("odd", -60)) // an offset the binary form cannot hold
	for _, a := range []any{
		struct{}{}, valuer{err: bad}, loop{}, marshaler{err: bad},
		broken, []time.Time{broken}, runtime.TstzRange{Lower: broken}, runtime.TstzRange{Upper: broken},
	} {
		if _, ok := entryKey("p:", "", "q", []any{a}, nil, nil); ok {
			t.Fatalf("%T keyed", a)
		}
	}
}

func TestTagsAndGenerationsAreInTheKey(t *testing.T) {
	a, _ := entryKey("p:", "", "q", nil, []string{"t"}, [][]byte{[]byte("1")})
	b, _ := entryKey("p:", "", "q", nil, []string{"t"}, [][]byte{[]byte("2")})
	c, _ := entryKey("p:", "", "q", nil, []string{"u"}, [][]byte{[]byte("1")})
	d, _ := entryKey("p:", "", "q", nil, nil, nil)
	if a == b || a == c || a == d {
		t.Fatal("tags or generations do not change the key")
	}
}

func TestHitRowsOutOfRange(t *testing.T) {
	h := &hitRows{raw: [][][]byte{{[]byte("x")}}}
	if h.RawValues() != nil {
		t.Fatal("RawValues before Next")
	}
	h.Close()
	(*valueHit)(h).Close()
	if h.Err() != nil {
		t.Fatal("Err")
	}
	if (*valueHit)(h).Err() != nil {
		t.Fatal("Err")
	}
}

func TestAValueTimeItCannotEncodeIsNotStored(t *testing.T) {
	broken := time.Date(1, 1, 1, 0, 0, 0, 0, time.FixedZone("odd", -60))
	if _, ok := appendValueRow(nil, []any{broken}); ok {
		t.Fatal("encoded a time whose zone the binary form cannot hold")
	}
}

// An entry comes back from a server storm does not control. Whatever it
// holds, decode returns rows or errCorrupt; it never panics or over-reads.
func FuzzDecode(f *testing.F) {
	b := appendByteRow(header(shapeBytes), [][]byte{[]byte("a"), nil, {}})
	v, _ := appendValueRow(header(shapeValues), []any{int64(1), 2.0, true, false, []byte("x"), "s", time.Unix(0, 0), nil})
	f.Add(b)
	f.Add(v)
	f.Fuzz(func(t *testing.T, p []byte) {
		h, err := decode(p)
		if err != nil {
			return
		}
		r := h.rows()
		for r.Next() {
			_ = r.RawValues()
			if vr, ok := r.(runtime.ValueRows); ok {
				_ = vr.Values()
			}
		}
	})
}
