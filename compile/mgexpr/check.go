// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgexpr

import (
	"strings"

	"github.com/gsoultan/storm/compile/mongo"
	"github.com/gsoultan/storm/schema"
)

// Check is a CHECK constraint's expression as an aggregation expression a
// validator's $expr accepts: true unless the SQL expression is FALSE, which is
// when a CHECK fails — NULL, SQL's UNKNOWN, passes.
func Check(t *schema.Table, expr string) (string, error) {
	n, err := parse(t, expr)
	if err != nil {
		return "", err
	}
	if err := boolean(n); err != nil {
		return "", err
	}
	return `{"$ne":[` + agg(t, n) + `,false]}`, nil
}

// agg is n as an aggregation expression whose value is SQL's: true, false or
// null for a condition, the value or null otherwise. A missing field is read
// as null, never as "missing", which compares differently.
func agg(t *schema.Table, n *node) string {
	switch n.kind {
	case 'c':
		return `{"$ifNull":[` + mongo.Str("$"+mongo.Field(t, n.col.Name)) + `,null]}`
	case 'l':
		return n.lit
	case 'f':
		switch n.op {
		case "length":
			return strict([]string{agg(t, n.kids[0])}, `{"$strLenCP":"$$a"}`)
		case "abs":
			return `{"$abs":` + agg(t, n.kids[0]) + `}`
		default: // coalesce
			return `{"$ifNull":[` + list(t, n.kids) + `]}`
		}
	case 'a':
		op := map[string]string{"+": "$add", "-": "$subtract", "*": "$multiply"}[n.op]
		return `{"` + op + `":[` + list(t, n.kids) + `]}`
	case 'm':
		op := map[string]string{"=": "$eq", "<>": "$ne", "<": "$lt", "<=": "$lte", ">": "$gt", ">=": "$gte"}[n.op]
		return strict([]string{agg(t, n.kids[0]), agg(t, n.kids[1])}, `{"`+op+`":["$$a","$$b"]}`)
	case '&':
		return logic(t, n.kids, "false", "true")
	case '|':
		return logic(t, n.kids, "true", "false")
	case '!':
		return strict([]string{agg(t, n.kids[0])}, `{"$not":["$$a"]}`)
	case 'z':
		op := "$eq"
		if n.neg {
			op = "$ne"
		}
		return `{"` + op + `":[` + agg(t, n.kids[0]) + `,null]}`
	default: // 'k'
		m := `{"$regexMatch":{"input":"$$a","regex":` + mongo.Str(likeRegex(n.str)) + `,"options":"s"}}`
		if n.neg {
			m = `{"$not":[` + m + `]}`
		}
		return strict([]string{agg(t, n.kids[0])}, m)
	}
}

func list(t *schema.Table, ns []*node) string {
	parts := make([]string, len(ns))
	for i, k := range ns {
		parts[i] = agg(t, k)
	}
	return strings.Join(parts, ",")
}

// strict binds its operands to $$a, $$b and is null when any is null — how
// every SQL operator but IS, AND, OR and coalesce treats UNKNOWN.
func strict(operands []string, expr string) string {
	names := []string{"a", "b"}
	var vars, nulls []string
	for i, o := range operands {
		vars = append(vars, `"`+names[i]+`":`+o)
		nulls = append(nulls, `{"$eq":["$$`+names[i]+`",null]}`)
	}
	test := nulls[0]
	if len(nulls) > 1 {
		test = `{"$or":[` + strings.Join(nulls, ",") + `]}`
	}
	return `{"$let":{"vars":{` + strings.Join(vars, ",") + `},"in":{"$cond":[` + test + `,null,` + expr + `]}}}`
}

// logic is AND (dominant false) or OR (dominant true) in three values: the
// dominant value if any operand has it, else null if any is null, else the
// other.
func logic(t *schema.Table, kids []*node, dominant, other string) string {
	return `{"$let":{"vars":{"v":[` + list(t, kids) + `]},"in":{"$cond":[{"$in":[` + dominant +
		`,"$$v"]},` + dominant + `,{"$cond":[{"$in":[null,"$$v"]},null,` + other + `]}]}}}`
}

// likeRegex is a LIKE pattern as an anchored regular expression: % and _ are
// the wildcards, a backslash escapes the next character, everything else is
// literal.
func likeRegex(p string) string {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteByte('.')
		case '\\':
			if i+1 < len(p) {
				i++
				quoteByte(&b, p[i])
			}
		default:
			quoteByte(&b, c)
		}
	}
	b.WriteByte('$')
	return b.String()
}

func quoteByte(b *strings.Builder, c byte) {
	if strings.IndexByte(`\.+*?()|[]{}^$`, c) >= 0 {
		b.WriteByte('\\')
	}
	b.WriteByte(c)
}
