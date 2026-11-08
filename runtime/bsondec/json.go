// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package bsondec

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/gsoultan/storm/runtime"
)

// JSON decodes a json or jsonb column: the BSON value the JSON text was
// stored as, rendered back as JSON.
//
// A document comes back with its keys in stored order — the order they were
// written in, which jsonb does not keep and json does. A number comes back as
// the BSON type it was stored as: an integer as digits, a double in Go's
// shortest form. A value no JSON text stored — a date, binary, an ObjectId, a
// decimal, written by something other than storm — is rendered in the plainest
// JSON that says what it is (an RFC 3339 string, base64, hex, the decimal's
// digits) rather than refused, because the document is the caller's.
func JSON(b []byte) (runtime.JSON, error) {
	if len(b) < 1 {
		return nil, shape("a JSON value", b)
	}
	out, err := appendJSON(nil, b[0], b[1:])
	if err != nil {
		return nil, err
	}
	return runtime.JSON(out), nil
}

func appendJSON(out []byte, t byte, v []byte) ([]byte, error) {
	switch t {
	case TDocument, TArray:
		open, close := byte('{'), byte('}')
		if t == TArray {
			open, close = '[', ']'
		}
		out = append(out, open)
		first := true
		err := Elements(v, func(et byte, name string, ev []byte) error {
			if !first {
				out = append(out, ',')
			}
			first = false
			if t == TDocument {
				out = appendQuoted(out, name)
				out = append(out, ':')
			}
			var err error
			out, err = appendJSON(out, et, ev)
			return err
		})
		if err != nil {
			return nil, err
		}
		return append(out, close), nil
	case TString:
		s, err := Str(append([]byte{t}, v...))
		if err != nil {
			return nil, err
		}
		return appendQuoted(out, s), nil
	case TInt32:
		return strconv.AppendInt(out, int64(int32(binary.LittleEndian.Uint32(v))), 10), nil
	case TInt64:
		return strconv.AppendInt(out, int64(binary.LittleEndian.Uint64(v)), 10), nil
	case TDouble:
		f := math.Float64frombits(binary.LittleEndian.Uint64(v))
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return appendQuoted(out, strconv.FormatFloat(f, 'g', -1, 64)), nil
		}
		return strconv.AppendFloat(out, f, 'g', -1, 64), nil
	case TBool:
		return strconv.AppendBool(out, v[0] != 0), nil
	case TNull, 0x06:
		return append(out, "null"...), nil
	case TDateTime:
		ms := int64(binary.LittleEndian.Uint64(v))
		return appendQuoted(out, time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)), nil
	case TBinary:
		_, data, err := binaryOf(append([]byte{t}, v...))
		if err != nil {
			return nil, err
		}
		return appendQuoted(out, base64.StdEncoding.EncodeToString(data)), nil
	case 0x07:
		return appendQuoted(out, hex.EncodeToString(v)), nil
	case TDecimal:
		d, err := Decimal(append([]byte{t}, v...))
		if err != nil {
			return appendQuoted(out, "NaN"), nil
		}
		return append(out, d.String()...), nil
	}
	return nil, shape("a JSON value", append([]byte{t}, v...))
}

// appendQuoted writes s as a JSON string.
func appendQuoted(out []byte, s string) []byte {
	const hexd = "0123456789abcdef"
	out = append(out, '"')
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"':
			out = append(out, '\\', '"')
		case r == '\\':
			out = append(out, '\\', '\\')
		case r < 0x20:
			out = append(out, '\\', 'u', '0', '0', hexd[r>>4], hexd[r&0xf])
		case r == utf8.RuneError && n == 1:
			out = append(out, "�"...)
		default:
			out = append(out, s[i:i+n]...)
		}
		i += n
	}
	return append(out, '"')
}
