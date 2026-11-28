// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlitedrv

import (
	"strconv"
	"time"
)

// jsonList writes a Go slice as the JSON array compile/sqlite unpacks with
// json_each, or reports that a is not a list this port binds.
//
// Hand-written rather than encoding/json, for the reason runtime/mydrv's is:
// the element types are a closed set, and marshalling by reflection on the hot
// path of every fetch plan is the cost the generated code exists to avoid.
//
// BYTES GO AS HEX and the lowering unhex()es them: a JSON document is text,
// and a uuid's bytes are not valid UTF-8.
func jsonList(a any) (string, bool) {
	a = derefList(a)
	b := make([]byte, 0, 64)
	b = append(b, '[')
	switch v := a.(type) {
	case []int64:
		for i, x := range v {
			b = sep(b, i)
			b = strconv.AppendInt(b, x, 10)
		}
	case []int32:
		for i, x := range v {
			b = sep(b, i)
			b = strconv.AppendInt(b, int64(x), 10)
		}
	case []int16:
		for i, x := range v {
			b = sep(b, i)
			b = strconv.AppendInt(b, int64(x), 10)
		}
	case []int:
		for i, x := range v {
			b = sep(b, i)
			b = strconv.AppendInt(b, int64(x), 10)
		}
	case []float64:
		for i, x := range v {
			b = sep(b, i)
			b = strconv.AppendFloat(b, x, 'g', -1, 64)
		}
	case []string:
		for i, x := range v {
			b = sep(b, i)
			b = appendString(b, x)
		}
	case [][16]byte:
		for i, x := range v {
			b = sep(b, i)
			b = appendHex(b, x[:])
		}
	case [][]byte:
		for i, x := range v {
			b = sep(b, i)
			b = appendHex(b, x)
		}
	case []bool:
		for i, x := range v {
			b = sep(b, i)
			b = strconv.AppendBool(b, x)
		}
	case []time.Time:
		for i, x := range v {
			b = sep(b, i)
			b = append(b, '"')
			b = x.UTC().AppendFormat(b, TimestampFormat)
			b = append(b, '"')
		}
	default:
		return "", false
	}
	return string(append(b, ']')), true
}

// derefList follows the pointer a generated binder passes for a list: the
// binders hand every argument by address, which database/sql follows for a
// scalar and not for a slice it has no conversion for.
func derefList(a any) any {
	switch p := a.(type) {
	case *[]int64:
		if p != nil {
			return *p
		}
	case *[]int32:
		if p != nil {
			return *p
		}
	case *[]int16:
		if p != nil {
			return *p
		}
	case *[]int:
		if p != nil {
			return *p
		}
	case *[]float64:
		if p != nil {
			return *p
		}
	case *[]string:
		if p != nil {
			return *p
		}
	case *[][16]byte:
		if p != nil {
			return *p
		}
	case *[][]byte:
		if p != nil {
			return *p
		}
	case *[]bool:
		if p != nil {
			return *p
		}
	case *[]time.Time:
		if p != nil {
			return *p
		}
	}
	return a
}

func sep(b []byte, i int) []byte {
	if i > 0 {
		return append(b, ',')
	}
	return b
}

const hexDigits = "0123456789ABCDEF"

func appendHex(b, v []byte) []byte {
	b = append(b, '"')
	for _, c := range v {
		b = append(b, hexDigits[c>>4], hexDigits[c&0x0f])
	}
	return append(b, '"')
}

// appendString escapes what JSON requires and nothing else. An unescaped
// quote would end the string early and turn the rest of a caller's value into
// JSON syntax — the injection the binding path exists to avoid, one layer in.
func appendString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c == '\n':
			b = append(b, '\\', 'n')
		case c == '\r':
			b = append(b, '\\', 'r')
		case c == '\t':
			b = append(b, '\\', 't')
		case c < 0x20:
			b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0x0f])
		default:
			b = append(b, c)
		}
	}
	return append(b, '"')
}
