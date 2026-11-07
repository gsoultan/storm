// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mongo

// Operator lowering.
//
// Every fragment is split where its hole goes, as compile/pgsql's are: the
// text before the value and the text after it. An operator that binds no value
// (IsNull) is all prefix.
//
// The interesting entries are the ones where MongoDB's answer differs from
// SQL's, because the generated API promises SQL's:
//
//   - NotEq. SQL's `a <> $1` is UNKNOWN for a NULL a, so the row is not
//     returned. MongoDB's `$ne` matches a missing or null field. Lowered to
//     `$nin: [v, null]`, which excludes exactly the rows SQL excludes.
//   - NotIn, for the same reason: `$nin` plus an explicit not-null.
//   - IsNull matches a field that is null OR absent. A document store has no
//     column a row can lack, so absence is how a NULL is usually stored, and
//     storm writes it that way.
//   - The text matches are regular expressions over a value the generated
//     code escapes, never over the caller's raw text — see runtime/mgsplice.
//
// Not and NotAny are deliberately missing. `$nor` matches a document whose
// field is null where SQL's NOT over the same comparison is UNKNOWN, so a
// negated predicate would return rows the SQL back ends do not. Refusing the
// constructor is honest; translating it is a silent difference.
var frags = map[string][2]string{
	"Eq":        {`{"$eq":`, `}`},
	"NotEq":     {`{"$nin":[`, `,null]}`},
	"Gt":        {`{"$gt":`, `}`},
	"Gte":       {`{"$gte":`, `}`},
	"Lt":        {`{"$lt":`, `}`},
	"Lte":       {`{"$lte":`, `}`},
	"In":        {`{"$in":`, `}`},
	"HasPrefix": {`{"$regex":`, `}`},
	"HasSuffix": {`{"$regex":`, `}`},
	"Contains":  {`{"$regex":`, `}`},
	"Like":      {`{"$regex":`, `}`},
	"ILike":     {`{"$regex":`, `,"$options":"i"}`},
}

// argless are the operators that bind nothing.
var argless = map[string]string{
	"IsNull":    `null`,
	"IsNotNull": `{"$ne":null}`,
	// A json column whose value is null, absent, {} or [] — the four texts
	// PostgreSQL's IsEmpty compares with, as BSON.
	"JSONIsEmpty": `{"$in":[null,{},[]]}`,
}

// The array operators, on a column that holds a BSON array. Each states its
// SQL meaning, because MongoDB's array matching is element-wise and a filter
// that reads the same can answer differently:
//
//   - ArrayContains is `@>`: every bound element is in the column — $all.
//     $all over an empty list matches nothing, where every array contains
//     the empty one; the generated method sends IsNotNull instead.
//   - ArrayContainedBy is `<@`: no element of the column is outside the list.
//     "$type":"array" keeps a NULL column out, as SQL's NULL <@ x does.
//   - ArrayOverlaps is `&&`: the two share an element — $in.
var arrayFrags = map[string][2]string{
	"ArrayContains":    {`{"$all":`, `}`},
	"ArrayContainedBy": {`{"$type":"array","$not":{"$elemMatch":{"$nin":`, `}}}`},
	"ArrayOverlaps":    {`{"$in":`, `}`},
}

// The json operators bind a FILTER rather than a value: runtime/mgsplice
// builds it from the caller's document or keys, with the column's path
// already in it, so the fragment is only the connective it sits in.
var jsonFrags = map[string][2]string{
	"JSONContains": {`{"$and":`, `}`},
	"HasAnyKey":    {`{"$or":`, `}`},
	"HasAllKeys":   {`{"$and":`, `}`},
}

// Frag lowers one comparison on field. ok is false for an operator this back
// end has no lowering for; hasArg is false for one that binds no value, in
// which case the whole predicate is a.
func Frag(op, field string) (a, b string, hasArg, ok bool) {
	key := Str(field) + ":"
	if op == "NotIn" {
		// Two conditions on one field: the list, and that the field is set.
		// One object, so it stays a single predicate to the splicer.
		return `{` + key + `{"$nin":`, `,"$ne":null}}`, true, true
	}
	if t, found := argless[op]; found {
		return `{` + key + t + `}`, "", false, true
	}
	if f, found := arrayFrags[op]; found {
		return `{` + key + f[0], f[1] + `}`, true, true
	}
	if f, found := jsonFrags[op]; found {
		return f[0], f[1], true, true
	}
	f, found := frags[op]
	if !found {
		return "", "", false, false
	}
	return `{` + key + f[0], f[1] + `}`, true, true
}

// Ops lists the operators Frag lowers, for a generator that wants to offer
// exactly those and no others.
func Ops() []string {
	return []string{"Eq", "NotEq", "Gt", "Gte", "Lt", "Lte", "In", "NotIn",
		"IsNull", "IsNotNull", "HasPrefix", "HasSuffix", "Contains", "Like", "ILike",
		"ArrayContains", "ArrayContainedBy", "ArrayOverlaps",
		"JSONContains", "HasAnyKey", "HasAllKeys", "JSONIsEmpty"}
}

// The connectives the splicer writes around a group. Prefix notation where
// SQL is infix, which is the whole of the difference: `(a AND b)` is open,
// a, sep, b, close for both.
const (
	AndOpen   = `{"$and":[`
	OrOpen    = `{"$or":[`
	GroupSep  = `,`
	GroupEnd  = `]}`
	NoFilter  = `{}`
	SortOpen  = `{`
	SortSep   = `,`
	SortClose = `}`
)

// SortTerm is one sort key. MongoDB places null and missing values FIRST in
// ascending order, where PostgreSQL places them last; the generated comment on
// Asc says so, because translating it would need a computed sort key and lose
// the index.
func SortTerm(field string, desc bool) string {
	if desc {
		return Str(field) + ":-1"
	}
	return Str(field) + ":1"
}
