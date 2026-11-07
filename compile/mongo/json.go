// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mongo

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gsoultan/storm/schema"
)

// Str renders s as a JSON string literal.
//
// Hand-written rather than encoding/json so the output is byte-stable and
// carries no HTML escaping: the text is a statement and a cache key, and two
// spellings of one string would be two statements.
func Str(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte("0123456789abcdef"[r>>4])
			b.WriteByte("0123456789abcdef"[r&0xf])
		case r == utf8.RuneError && n == 1:
			b.WriteString(`�`)
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	b.WriteByte('"')
	return b.String()
}

// Arg is the hole for the n-th bound value, counted from 1.
//
// An object rather than a bare marker string so it can never collide with a
// value: a caller's string is bound as BSON, never spliced into this text, and
// no JSON a back end writes uses the key "$arg".
func Arg(n int) string { return `{"$arg":` + strconv.Itoa(n) + `}` }

// IDField is the field MongoDB keys every document on.
const IDField = "_id"

// Field is the stored name of a column: the column's own name, except that
// the primary key is stored as _id — the column itself for a single-column
// key, and a field of an _id subdocument for a composite one.
//
// Mapping the key onto _id rather than beside it is what makes a lookup by key
// use the index every collection already has, instead of a second unique index
// that would double the write cost of every insert. A composite key is a
// subdocument whose fields are in key order, so equality on the whole key is
// one comparison on that index.
func Field(t *schema.Table, col string) string {
	if len(t.PrimaryKey) == 1 && t.PrimaryKey[0] == col {
		return IDField
	}
	for _, k := range t.PrimaryKey {
		if k == col {
			return IDField + "." + col
		}
	}
	return col
}

// KeyFilter is the filter that addresses one document by its whole key, with
// the key's values in holes first, first+1, ... in key order.
func KeyFilter(t *schema.Table, first int) string {
	return `{"_id":` + KeyValue(t, first) + `}`
}

// KeyValue is the _id value: the hole itself for a single-column key, and a
// subdocument of holes in key order for a composite one.
func KeyValue(t *schema.Table, first int) string {
	if len(t.PrimaryKey) == 1 {
		return Arg(first)
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range t.PrimaryKey {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(Str(k))
		b.WriteByte(':')
		b.WriteString(Arg(first + i))
	}
	b.WriteByte('}')
	return b.String()
}

// Fields maps Field over a column list.
func Fields(t *schema.Table, cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = Field(t, c)
	}
	return out
}

// Projection is a find's projection over fields, in order. The order is
// load-bearing: runtime/mgdrv hands a row's values back in the order the
// projection names them, which is the order the generated scanner reads.
func Projection(fields []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(Str(f))
		b.WriteString(":1")
	}
	b.WriteByte('}')
	return b.String()
}
