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

// Partial is a partial index's WHERE as a partialFilterExpression document.
//
// That language is a query's, cut down to what an index can test: equality,
// $gt/$gte/$lt/$lte, $type, $in, $and and $or. Each is translated where it
// selects exactly the rows SQL's predicate is TRUE for — a comparison never
// matches a missing field, which is SQL's NULL, and IS NOT NULL is a $type
// test since $exists would admit an explicit null. Negation has no form.
func Partial(t *schema.Table, where string) (string, error) {
	n, err := parse(t, where)
	if err != nil {
		return "", err
	}
	return filter(t, n)
}

func filter(t *schema.Table, n *node) (string, error) {
	switch n.kind {
	case '&', '|':
		op := "$and"
		if n.kind == '|' {
			op = "$or"
		}
		if n.op == "in" {
			if f, ok := in(t, n); ok {
				return f, nil
			}
		}
		parts := make([]string, len(n.kids))
		for i, k := range n.kids {
			f, err := filter(t, k)
			if err != nil {
				return "", err
			}
			parts[i] = f
		}
		return `{"` + op + `":[` + strings.Join(parts, ",") + `]}`, nil
	case 'c':
		if n.col.Type.Name == schema.TypeBool {
			return `{` + field(t, n) + `:true}`, nil
		}
	case '!':
		if k := n.kids[0]; k.kind == 'c' && k.col.Type.Name == schema.TypeBool {
			return `{` + field(t, k) + `:false}`, nil
		}
	case 'z':
		if k := n.kids[0]; k.kind == 'c' {
			if !n.neg {
				return `{` + field(t, k) + `:null}`, nil
			}
			bt, _ := mongo.BSONType(k.col)
			return `{` + field(t, k) + `:{"$type":` + mongo.Str(bt) + `}}`, nil
		}
	case 'm':
		a, b, op := n.kids[0], n.kids[1], n.op
		if a.kind == 'l' && b.kind == 'c' {
			a, b = b, a
			op = map[string]string{"=": "=", "<>": "<>", "<": ">", "<=": ">=", ">": "<", ">=": "<="}[op]
		}
		if a.kind == 'c' && b.kind == 'l' && b.cls != "null" && op != "<>" {
			if op == "=" {
				return `{` + field(t, a) + `:` + value(b) + `}`, nil
			}
			ops := map[string]string{"<": "$lt", "<=": "$lte", ">": "$gt", ">=": "$gte"}
			return `{` + field(t, a) + `:{"` + ops[op] + `":` + value(b) + `}}`, nil
		}
	}
	return "", fail("a predicate partialFilterExpression cannot state")
}

// in is col IN (literals...) as one $in, when it is that.
func in(t *schema.Table, n *node) (string, bool) {
	var col *node
	var vals []string
	for _, e := range n.kids {
		a, b := e.kids[0], e.kids[1]
		if a.kind != 'c' || b.kind != 'l' || b.cls == "null" || col != nil && a.col != col.col {
			return "", false
		}
		col = a
		vals = append(vals, value(b))
	}
	return `{` + field(t, col) + `:{"$in":[` + strings.Join(vals, ",") + `]}}`, true
}

func field(t *schema.Table, n *node) string { return mongo.Str(mongo.Field(t, n.col.Name)) }

// value is a literal as a query states it: a string is a string there, not
// a field path, so it needs no $literal.
func value(n *node) string {
	if n.cls == "str" || n.cls == "text" {
		return mongo.Str(n.str)
	}
	return n.lit
}
