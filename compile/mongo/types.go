// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mongo

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// BSONType is the $jsonSchema bsonType a column is stored as, or ok=false for
// a type this back end does not carry.
//
// An array is a BSON array of its element type (ElemType says which). json
// and jsonb are stored as the BSON value the JSON text denotes — a document,
// an array, a string, a number, a bool — rather than as text, so the fields
// inside are queryable as what they are; their bsonType is "" because the
// column admits any of those. The refusals are the PostgreSQL-only types —
// ranges, network addresses, intervals, tsvector, hstore — and arrays of
// anything but a scalar this back end carries.
func BSONType(c *schema.Column) (string, bool) {
	if c.Type.Array {
		if _, ok := ElemType(c); !ok {
			return "", false
		}
		return "array", true
	}
	return scalarType(c.Type)
}

// ElemType is an array column's element bsonType.
func ElemType(c *schema.Column) (string, bool) {
	if !c.Type.Array {
		return "", false
	}
	e := c.Type
	e.Array = false
	switch e.Name {
	case schema.TypeJSON, schema.TypeJSONB, schema.TypeNumeric, schema.TypeBytea:
		// An array of documents is an embedded relation, not a column; an
		// array of decimals or byte strings has no decoder yet.
		return "", false
	}
	return scalarType(e)
}

func scalarType(t schema.Type) (string, bool) {
	if t.Enum {
		return "string", true
	}
	switch t.Name {
	case schema.TypeBool:
		return "bool", true
	case schema.TypeInt2, schema.TypeInt4:
		return "int", true
	case schema.TypeInt8:
		return "long", true
	case schema.TypeFloat4, schema.TypeFloat8:
		return "double", true
	case schema.TypeText, schema.TypeVarchar:
		return "string", true
	case schema.TypeBytea, schema.TypeUUID:
		return "binData", true
	case schema.TypeTimestamptz, schema.TypeDate:
		// A date is stored as midnight UTC: BSON has one temporal type, and a
		// date that drifted with a time zone would change which day it is.
		return "date", true
	case schema.TypeNumeric:
		return "decimal", true
	case schema.TypeJSON, schema.TypeJSONB:
		return "", true
	}
	return "", false
}

// IsJSON reports a json or jsonb column.
func IsJSON(c *schema.Column) bool {
	return !c.Type.Array && (c.Type.Name == schema.TypeJSON || c.Type.Name == schema.TypeJSONB)
}

// DefaultKind classifies a column default by who computes it.
type DefaultKind uint8

const (
	// DefaultNone is a column with no default.
	DefaultNone DefaultKind = iota
	// DefaultServerTime is now(): the SERVER's clock, written with
	// $currentDate, never the client's.
	DefaultServerTime
	// DefaultUUIDv4 and DefaultUUIDv7 are generated client-side, as on
	// MySQL: MongoDB has no uuid generator a write can call.
	DefaultUUIDv4
	DefaultUUIDv7
	// DefaultLiteral is a constant, written on insert.
	DefaultLiteral
	// DefaultUnsupported is any other expression. SQL text cannot be
	// evaluated by a document store, and guessing at it is refused.
	DefaultUnsupported
)

// ClassifyDefault reports how a column's default is produced on this back end,
// and for a literal, its JSON form.
func ClassifyDefault(c *schema.Column) (DefaultKind, string) {
	d := plainDefault(c.Default)
	switch strings.ToLower(d) {
	case "", "null":
		return DefaultNone, ""
	case "now()", "current_timestamp", "transaction_timestamp()":
		if c.Type.Name == schema.TypeTimestamptz {
			return DefaultServerTime, ""
		}
		return DefaultUnsupported, ""
	case "gen_random_uuid()":
		return DefaultUUIDv4, ""
	case "uuidv7()":
		return DefaultUUIDv7, ""
	case "true", "false":
		if c.Type.Name == schema.TypeBool {
			return DefaultLiteral, strings.ToLower(d)
		}
		return DefaultUnsupported, ""
	}
	if lit, ok := sqlStringLiteral(d); ok {
		if c.Type.Array {
			if j, ok := arrayLiteral(lit, c); ok {
				return DefaultLiteral, j
			}
			return DefaultUnsupported, ""
		}
		if IsJSON(c) {
			if j, ok := JSONLiteral(lit); ok {
				return DefaultLiteral, j
			}
			return DefaultUnsupported, ""
		}
		t, _ := BSONType(c)
		if t != "string" {
			return DefaultUnsupported, ""
		}
		return DefaultLiteral, Str(lit)
	}
	if isNumber(d) && !c.Type.Array {
		return numberLiteral(c.Type.Name, d)
	}
	return DefaultUnsupported, ""
}

// plainDefault is a default with what changes nothing taken off: spaces,
// enclosing parentheses — PostgreSQL's catalogue prints (0) and ('x') — and a
// cast after a number or a boolean (sqlStringLiteral reads a string's with
// it). 0::integer is 0 to the column it defaults, as true::boolean is true;
// the column's type is what the value is stored as either way.
func plainDefault(d string) string {
	for {
		d = strings.TrimSpace(d)
		if len(d) >= 2 && d[0] == '(' && d[len(d)-1] == ')' && balanced(d[1:len(d)-1]) {
			d = d[1 : len(d)-1]
			continue
		}
		if i := strings.LastIndex(d, "::"); i > 0 && d[0] != '\'' {
			// Only off a number or a boolean: now()::date is midnight, not now.
			v := plainDefault(d[:i])
			if l := strings.ToLower(v); isNumber(v) || l == "true" || l == "false" {
				return v
			}
		}
		return d
	}
}

// balanced reports whether s's parentheses pair up, so (a)(b) is not read
// as one parenthesised a)(b.
func balanced(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// sqlStringLiteral unquotes 'it”s' to it's, optionally followed by a cast.
func sqlStringLiteral(s string) (string, bool) {
	if len(s) < 2 || s[0] != '\'' {
		return "", false
	}
	var out []byte
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			out = append(out, s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			out = append(out, '\'')
			i++
			continue
		}
		rest := s[i+1:]
		if rest == "" || (len(rest) > 2 && rest[:2] == "::") {
			return string(out), true
		}
		return "", false
	}
	return "", false
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	i, dot, digits := 0, false, false
	if s[0] == '-' {
		i = 1
	}
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits = true
		case c == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return digits
}

// arrayLiteral reads PostgreSQL's array literal — '{}' or '{a,b}' — as a JSON
// array, for text and integer elements without quoting or NULLs. Anything
// richer is refused rather than guessed at.
func arrayLiteral(lit string, c *schema.Column) (string, bool) {
	if len(lit) < 2 || lit[0] != '{' || lit[len(lit)-1] != '}' {
		return "", false
	}
	body := lit[1 : len(lit)-1]
	if body == "" {
		return "[]", true
	}
	et, _ := ElemType(c)
	var b strings.Builder
	b.WriteByte('[')
	for i, e := range strings.Split(body, ",") {
		if e == "" || strings.ContainsAny(e, `"\{} `) || strings.EqualFold(e, "null") {
			return "", false
		}
		if i > 0 {
			b.WriteByte(',')
		}
		switch et {
		case "string":
			b.WriteString(Str(e))
		case "int", "long", "double":
			if !isNumber(e) {
				return "", false
			}
			b.WriteString(e)
		case "bool":
			switch strings.ToLower(e) {
			case "t", "true":
				b.WriteString("true")
			case "f", "false":
				b.WriteString("false")
			default:
				return "", false
			}
		default:
			return "", false
		}
	}
	b.WriteByte(']')
	return b.String(), true
}

// JSONLiteral re-renders a JSON text in this package's canonical spelling —
// compact, keys in their written order — or ok=false for text that is not
// one JSON value. A literal default is written into a command, so it must be
// JSON the driver compiles, never SQL.
func JSONLiteral(s string) (string, bool) {
	var b strings.Builder
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	if !canon(d, &b) {
		return "", false
	}
	if _, err := d.Token(); err != io.EOF {
		return "", false
	}
	return b.String(), true
}

func canon(d *json.Decoder, b *strings.Builder) bool {
	tok, err := d.Token()
	if err != nil {
		return false
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			b.WriteByte('{')
			for i := 0; d.More(); i++ {
				k, err := d.Token()
				ks, ok := k.(string)
				if err != nil || !ok {
					return false
				}
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(Str(ks))
				b.WriteByte(':')
				if !canon(d, b) {
					return false
				}
			}
			b.WriteByte('}')
		case '[':
			b.WriteByte('[')
			for i := 0; d.More(); i++ {
				if i > 0 {
					b.WriteByte(',')
				}
				if !canon(d, b) {
					return false
				}
			}
			b.WriteByte(']')
		default:
			return false
		}
		_, err := d.Token() // the closing delimiter
		return err == nil
	case string:
		b.WriteString(Str(v))
	case json.Number:
		b.WriteString(string(v))
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case nil:
		b.WriteString("null")
	}
	return true
}

// numberLiteral types a numeric default the way the validator stores the
// column: an int8 as a 64-bit integer — in canonical extended JSON, since a
// bare small number would be written as int32 and refused — and a float as a
// double, which a bare integer would not be.
func numberLiteral(typ, d string) (DefaultKind, string) {
	switch typ {
	case schema.TypeInt2, schema.TypeInt4:
		if strings.Contains(d, ".") {
			return DefaultUnsupported, ""
		}
		n, err := strconv.ParseInt(d, 10, 32)
		if err != nil || (typ == schema.TypeInt2 && (n < -32768 || n > 32767)) {
			return DefaultUnsupported, ""
		}
		return DefaultLiteral, d
	case schema.TypeInt8:
		if _, err := strconv.ParseInt(d, 10, 64); err != nil {
			return DefaultUnsupported, ""
		}
		return DefaultLiteral, `{"$numberLong":` + Str(d) + `}`
	case schema.TypeFloat4, schema.TypeFloat8:
		if !strings.Contains(d, ".") {
			d += ".0"
		}
		return DefaultLiteral, d
	}
	return DefaultUnsupported, ""
}
