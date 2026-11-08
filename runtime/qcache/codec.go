// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package qcache

import (
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// An entry is a header and then its rows, each a column count and the cells:
//
//	entry  = version shape row*
//	row    = uvarint(ncols) cell*
//	cell   = (shape b) uvarint(len+1) bytes | 0 for NULL
//	       | (shape v) tag payload
//
// Two shapes because runtime.Rows has two: wire bytes, and the decoded
// driver.Value a database/sql adapter hands back (runtime.ValueRows). An entry
// replays in the shape it was recorded in, so the generated package reading it
// cannot tell a hit from the database.
const (
	codecVersion byte = 1
	shapeBytes   byte = 'b'
	shapeValues  byte = 'v'
)

// Value cell tags: exactly the types runtime.ValueRows promises.
const (
	valNil byte = iota
	valInt64
	valFloat64
	valFalse
	valTrue
	valBytes
	valString
	valTime
)

var errCorrupt = errors.New("qcache: cached entry does not decode; treated as a miss")

func header(shape byte) []byte { return append(make([]byte, 0, 256), codecVersion, shape) }

func appendByteRow(b []byte, row [][]byte) []byte {
	b = binary.AppendUvarint(b, uint64(len(row)))
	for _, c := range row {
		if c == nil {
			b = append(b, 0)
			continue
		}
		b = binary.AppendUvarint(b, uint64(len(c))+1)
		b = append(b, c...)
	}
	return b
}

// appendValueRow returns ok false for a value outside driver.Value's set, and
// the entry is then not stored.
func appendValueRow(b []byte, row []any) ([]byte, bool) {
	b = binary.AppendUvarint(b, uint64(len(row)))
	for _, v := range row {
		switch x := v.(type) {
		case nil:
			b = append(b, valNil)
		case int64:
			b = binary.AppendVarint(append(b, valInt64), x)
		case float64:
			b = binary.BigEndian.AppendUint64(append(b, valFloat64), math.Float64bits(x))
		case bool:
			if x {
				b = append(b, valTrue)
			} else {
				b = append(b, valFalse)
			}
		case []byte:
			if x == nil {
				b = append(b, valNil)
				continue
			}
			b = appendStr(append(b, valBytes), string(x))
		case string:
			b = appendStr(append(b, valString), x)
		case time.Time:
			raw, err := x.MarshalBinary()
			if err != nil {
				return b, false
			}
			b = appendStr(append(b, valTime), string(raw))
		default:
			return b, false
		}
	}
	return b, true
}

// decode turns an entry back into rows. Cells alias p: nothing is copied, and
// p is never written again, so a hit costs the read and two slices.
func decode(p []byte) (*hitRows, error) {
	if len(p) < 2 || p[0] != codecVersion {
		return nil, errCorrupt
	}
	shape := p[1]
	r := &hitRows{values: shape == shapeValues}
	rest := p[2:]
	switch shape {
	case shapeBytes:
		for len(rest) > 0 {
			n, k := binary.Uvarint(rest)
			if k <= 0 || n > uint64(len(rest)) {
				return nil, errCorrupt
			}
			rest = rest[k:]
			row := make([][]byte, n)
			for i := range row {
				l, k := binary.Uvarint(rest)
				if k <= 0 {
					return nil, errCorrupt
				}
				rest = rest[k:]
				if l == 0 {
					continue
				}
				l--
				if l > uint64(len(rest)) {
					return nil, errCorrupt
				}
				row[i] = rest[:l:l]
				rest = rest[l:]
			}
			r.raw = append(r.raw, row)
		}
	case shapeValues:
		for len(rest) > 0 {
			n, k := binary.Uvarint(rest)
			if k <= 0 || n > uint64(len(rest)) {
				return nil, errCorrupt
			}
			rest = rest[k:]
			row := make([]any, n)
			for i := range row {
				var ok bool
				if row[i], rest, ok = decodeValue(rest); !ok {
					return nil, errCorrupt
				}
			}
			r.vals = append(r.vals, row)
		}
	default:
		return nil, errCorrupt
	}
	return r, nil
}

func decodeValue(p []byte) (any, []byte, bool) {
	if len(p) == 0 {
		return nil, p, false
	}
	tag, p := p[0], p[1:]
	switch tag {
	case valNil:
		return nil, p, true
	case valInt64:
		n, k := binary.Varint(p)
		if k <= 0 {
			return nil, p, false
		}
		return n, p[k:], true
	case valFloat64:
		if len(p) < 8 {
			return nil, p, false
		}
		return math.Float64frombits(binary.BigEndian.Uint64(p)), p[8:], true
	case valFalse:
		return false, p, true
	case valTrue:
		return true, p, true
	case valBytes, valString, valTime:
		l, k := binary.Uvarint(p)
		if k <= 0 || l > uint64(len(p)-k) {
			return nil, p, false
		}
		s, rest := p[k:k+int(l):k+int(l)], p[k+int(l):]
		switch tag {
		case valBytes:
			return s, rest, true
		case valString:
			return string(s), rest, true
		}
		var t time.Time
		if err := t.UnmarshalBinary(s); err != nil {
			return nil, p, false
		}
		return t, rest, true
	}
	return nil, p, false
}
