// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlite

import (
	"strconv"

	"github.com/gsoultan/storm/v2/schema"
)

// jsonb containment, rebuilt from SQLite's JSON functions.
//
// PostgreSQL's @> is recursive: an object contains another when it has every
// one of its keys and each value contains the other's; an array contains
// another when every element of the other is contained by SOME element of it;
// a scalar contains only an equal scalar. SQLite has the pieces — json_type,
// json_each, and -> which answers a member as JSON text — and no containment.
//
// The recursion is unrolled at GENERATE time, because the pattern is a bound
// value and SQL cannot recurse over a value's shape without a recursive CTE,
// which cannot express "for every element there is some element". It is
// unrolled to containmentDepth levels of nesting in the PATTERN — `{"a":
// {"b": [1]}}` is three — which covers the documents people filter by. Below
// that depth two containers are compared for equality of their minified text:
// never a row PostgreSQL would exclude, and possibly one fewer than it would
// return. docs/DIALECTS.md says so.
//
// One more difference, in numbers: a scalar is compared as JSON TEXT, so 1 and
// 1.0 are different values here and equal ones in PostgreSQL.

// containmentDepth is how many levels of nesting are compared exactly.
const containmentDepth = 3

// jsonParam is the alias the bound pattern is read through. A fragment can
// place its parameter once, at the end of its first half, and containment
// needs the pattern many times — so the fragment opens a one-row source that
// names it.
const jsonParam = `"_storm_j"."p"`

// containsFrags are JSONContains and JSONContainedBy around the column.
func containsFrags(col string, by bool) (a, b string) {
	a, bv := col, jsonParam
	if by {
		a, bv = jsonParam, col
	}
	n := 0
	return "EXISTS (SELECT 1 FROM (SELECT " + Placeholder,
		` AS "p") AS "_storm_j" WHERE ` + contains(a, bv, 0, &n) + ")"
}

// contains is the SQL for "document a contains document b", both JSON text.
func contains(a, b string, depth int, n *int) string {
	if depth == containmentDepth {
		return "(json_type(" + a + ") = json_type(" + b + ") AND json(" + a + ") = json(" + b + "))"
	}
	*n++
	k := `"_storm_k` + strconv.Itoa(*n) + `"`
	e := `"_storm_e` + strconv.Itoa(*n) + `"`
	f := `"_storm_f` + strconv.Itoa(*n) + `"`
	object := "json_type(" + a + ") = 'object' AND NOT EXISTS (SELECT 1 FROM json_each(" + b + ") AS " + k +
		" WHERE NOT (json_type(" + a + ", " + k + ".fullkey) IS NOT NULL AND " +
		contains("("+a+" -> "+k+".fullkey)", "("+b+" -> "+k+".fullkey)", depth+1, n) + "))"
	array := "json_type(" + a + ") = 'array' AND NOT EXISTS (SELECT 1 FROM json_each(" + b + ") AS " + e +
		" WHERE NOT EXISTS (SELECT 1 FROM json_each(" + a + ") AS " + f + " WHERE " +
		contains("("+a+" -> "+f+".fullkey)", "("+b+" -> "+e+".fullkey)", depth+1, n) + "))"
	scalar := "(json_type(" + a + ") = json_type(" + b + ") AND json(" + a + ") = json(" + b + "))"
	if depth == 0 {
		// PostgreSQL's one exception to "the structures must match": a
		// top-level array contains a scalar that is one of its elements.
		scalar = "(" + scalar + " OR (json_type(" + a + ") = 'array' AND EXISTS (SELECT 1 FROM json_each(" +
			a + ") AS " + f + " WHERE json((" + a + " -> " + f + ".fullkey)) = json(" + b + "))))"
	}
	return "(CASE json_type(" + b + ") WHEN 'object' THEN " + object + " WHEN 'array' THEN " + array +
		" ELSE " + scalar + " END)"
}

// InFrag lowers In and NotIn: membership in ONE bound JSON array, unpacked by
// json_each, so the statement has one parameter whatever the list's length.
//
// keyType is the column's declared SQLite type, because what json_each yields
// has to be turned back into what the column stores: a uuid travels as hex in
// the document and is stored as 16 bytes.
//
// NOT IN over a subquery has PostgreSQL's NULL trap: a NULL in the list makes
// every row's answer NULL. The adapter never puts one there — a Go slice of
// keys holds none.
func InFrag(ident, keyType string, not bool) (a, b string) {
	op := " IN (SELECT "
	if not {
		op = " NOT IN (SELECT "
	}
	return ident + op + keyValue(keyType) + " FROM json_each(" + Placeholder, "))"
}

// ColumnType is the type a lowering NAMES for a key column: only whether the
// key is stored as bytes matters, because that is what decides how a
// json_each element is turned back into it. compile/liteddl spells the DDL.
func ColumnType(c *schema.Column) string {
	if c.Type.Array || c.Type.Enum {
		return "TEXT"
	}
	switch c.Type.Name {
	case schema.TypeUUID:
		return "UUID"
	case schema.TypeBytea:
		return "BLOB"
	case schema.TypeInt2, schema.TypeInt4, schema.TypeInt8:
		return "INTEGER"
	}
	return "TEXT"
}

// keyValue is a json_each element as the value the key column stores.
func keyValue(keyType string) string {
	switch keyType {
	case "UUID", "BLOB":
		return `unhex("value")`
	}
	return `"value"`
}
