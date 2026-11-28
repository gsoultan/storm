// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package sqlite lowers query structure to SQLite text.
//
// It began as a copy of compile/pgsql, because SQLite's grammar is closer to
// PostgreSQL's than any other target's: double-quoted identifiers, LIMIT and
// OFFSET, RETURNING, ON CONFLICT with a target and EXCLUDED, FILTER on an
// aggregate, NULLS FIRST/LAST, row values, window functions and WITH
// RECURSIVE. What differs is listed where it differs; the headline items:
//
//   - Parameters are `?1`, `?2`: SQLite's numbered form. A repeated number
//     binds once, as `$1` does in PostgreSQL, so a lowering that reuses a
//     parameter needs no re-thinking.
//   - There is no array. A list — In, NotIn, the roots of a traversal, the
//     parents of a batch load — is bound as ONE JSON array and unpacked by
//     json_each, so the statement still has one parameter whatever the list's
//     length, and stays one cached statement.
//   - There are no row locks; see LockSuffix.
//   - There is no LATERAL, no DISTINCT ON and no data-modifying WITH, so the
//     reads and writes built on them are not generated here.
//
// Output is byte-deterministic: same input, same bytes, always.
package sqlite

import "strings"

// Ident quotes an identifier. SQLite matches names case-insensitively for
// ASCII, but quoting still matters: it is what keeps a column called "order"
// from being a keyword.
func Ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Placeholder marks where a bound parameter goes inside a fragment. The
// runtime appends the ordinal, so `?` becomes `?3`; see runtime.SQLitePlaceholder.
const Placeholder = "?"

// SelectPrefix is everything before the WHERE clause of a row read.
func SelectPrefix(table string, cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = Ident(c)
	}
	return "SELECT " + strings.Join(q, ", ") + " FROM " + Ident(table)
}

// SoftDeleteWhere is the predicate that keeps marked rows out of a read.
//
// It is rendered as a DECLARED predicate, which the splice ANDs ahead of the
// caller's own — so a call site can narrow what it sees and has no way to widen
// it. That is the whole reason soft delete can be offered here at all: the
// hazard docs/CONCEPT.md rejects is "every query that forgets the predicate",
// and a compiled statement cannot forget.
func SoftDeleteWhere(col string) string { return Ident(col) + " IS NULL" }

// CountPrefix is everything before the WHERE clause of a count.
func CountPrefix(table string) string { return "SELECT count(*) FROM " + Ident(table) }

// OrderSuffix is everything after the WHERE clause: the ordering, then LIMIT
// with a placeholder the runtime numbers.
func OrderSuffix(orderBy string) string {
	return " ORDER BY " + orderBy + " LIMIT " + Placeholder
}

// ExistsSuffix caps an existence probe. Ordering an EXISTS is wasted work.
func ExistsSuffix() string { return " LIMIT 1" }

// ExistsPrefix projects nothing: an existence probe that selects real columns
// pays heap fetches and decoding for a boolean. `SELECT 1` with LIMIT 1 lets
// the planner stop at the first matching tuple — measured against the old
// full-projection + default-order shape, which sorted 16,667 matching rows in
// a top-N heapsort to return one.
func ExistsPrefix(table string) string { return "SELECT 1 FROM " + Ident(table) }

// DefaultOrderBy is the ordering used when the caller names none. A read
// without ORDER BY has no defined order, so paging one is a bug waiting for a
// plan change; the primary key is the cheapest total order available.
func DefaultOrderBy(primaryKey []string, fallback string) string {
	if len(primaryKey) == 0 {
		return Ident(fallback)
	}
	parts := make([]string, len(primaryKey))
	for i, k := range primaryKey {
		parts[i] = Ident(k)
	}
	return strings.Join(parts, ", ")
}

// frags is the SQL for each operator, split where its placeholder goes: A,
// then the ordinal the runtime appends, then B. An operator absent from this
// table is one SQLite cannot express, and asking for it is a generation error
// rather than a silently dropped predicate: the ranges, the arrays and
// full-text search have no SQLite form. jsonb containment is json.go's.
var frags = map[string]struct{ a, b string }{
	"Eq":    {" = " + Placeholder, ""},
	"NotEq": {" <> " + Placeholder, ""},
	"Gt":    {" > " + Placeholder, ""},
	"Gte":   {" >= " + Placeholder, ""},
	"Lt":    {" < " + Placeholder, ""},
	"Lte":   {" <= " + Placeholder, ""},
	// LIKE is case-SENSITIVE as PostgreSQL's is only when the connection sets
	// case_sensitive_like, which runtime/sqlitedrv's DSN does. Without it
	// SQLite's LIKE folds ASCII case, and Like would mean ILike.
	// ESCAPE '\' spells PostgreSQL's default, which SQLite does not have: a
	// pattern's \_ is a literal underscore there and two characters here.
	"Like": {" LIKE " + Placeholder, likeBackslash},
	// Both sides lowered, which folds ASCII only: SQLite's lower() does not
	// know Unicode case without the ICU extension.
	"ILike": {") LIKE lower(" + Placeholder, ")" + likeBackslash},
	// jsonb key tests over the bound JSON array of keys: some key, or every
	// key, names a member of the document. A document that is not an object
	// has no keys, and json_type() of a missing member is NULL.
	"HasAnyKey": {"(json_type(" + keyHolder + ") = 'object' AND EXISTS (SELECT 1 FROM json_each(" + Placeholder,
		") AS \"_storm_k\" WHERE json_type(" + keyHolder + ", " + memberPath + ") IS NOT NULL))"},
	"HasAllKeys": {"(json_type(" + keyHolder + ") = 'object' AND NOT EXISTS (SELECT 1 FROM json_each(" + Placeholder,
		") AS \"_storm_k\" WHERE json_type(" + keyHolder + ", " + memberPath + ") IS NULL))"},
	// The document's text against every pattern in one bound JSON array.
	"JSONTextLikeAny": {"EXISTS (SELECT 1 FROM json_each(" + Placeholder,
		") AS \"_storm_p\" WHERE COALESCE(" + keyHolder + ", '') LIKE \"_storm_p\".\"value\"" + likeBackslash + ")"},
	"JSONIsEmpty": {", '') IN ('', 'null', '[]', '{}')", ""},
	"NullOrEmpty": {", '') = ''", ""},
	"IsNull":      {" IS NULL", ""},
	"IsNotNull":   {" IS NOT NULL", ""},
	"EqLower":     {") = lower(" + Placeholder, ")"},
	// Literal prefix, suffix and substring matches; see compile/pgsql for why
	// the escaping happens on the server.
	"TextHasPrefix": {" LIKE (" + likeEscOpen + Placeholder, likeEscClose + " || '%') ESCAPE '!'"},
	"TextHasSuffix": {" LIKE ('%' || " + likeEscOpen + Placeholder, likeEscClose + ") ESCAPE '!'"},
	"TextContains":  {" LIKE ('%' || " + likeEscOpen + Placeholder, likeEscClose + " || '%') ESCAPE '!'"},
}

// keyHolder stands for the column where a fragment needs it somewhere other
// than at its start — inside a subquery that has to open with the parameter,
// or a second time. Frag substitutes it.
const keyHolder = "\x00col\x00"

// memberPath is the JSON path of the member a json_each key names, quoted so
// a key with a dot or a bracket in it is one member rather than a path.
const memberPath = `'$."' || replace("_storm_k"."value", '"', '\"') || '"'`

// likeBackslash makes a backslash LIKE's escape character, as it is by default
// in PostgreSQL and MySQL.
const likeBackslash = ` ESCAPE '\'`

// likeEscOpen and likeEscClose wrap a bound value so that LIKE ... ESCAPE '!'
// reads it literally. The escape character goes first, or the escapes the
// later replacements add would themselves be doubled.
const (
	likeEscOpen  = "replace(replace(replace("
	likeEscClose = ", '!', '!!'), '%', '!%'), '_', '!_')"
)

// prefixes wrap the IDENTIFIER for operators whose left side is an expression
// rather than the bare column.
var prefixes = map[string]string{
	"ILike":       "lower(",
	"EqLower":     "lower(",
	"JSONIsEmpty": "COALESCE(",
	"NullOrEmpty": "COALESCE(",
}

// Frag lowers one operator applied to one already-quoted identifier. ok is
// false when this back end has no lowering for the operator. In and NotIn are
// InFrag's: they need the column's type.
func Frag(op, ident string) (a, b string, ok bool) {
	f, ok := frags[op]
	if !ok {
		switch op {
		case "JSONContains", "JSONContainedBy":
			a, b = containsFrags(ident, op == "JSONContainedBy")
			return a, b, true
		}
		return "", "", false
	}
	// A fragment that names the column itself, on either side of its
	// placeholder, is a whole predicate; the rest follow the column.
	if strings.Contains(f.a, keyHolder) || strings.Contains(f.b, keyHolder) {
		return strings.ReplaceAll(f.a, keyHolder, ident), strings.ReplaceAll(f.b, keyHolder, ident), true
	}
	return prefixes[op] + ident + f.a, f.b, true
}

// Ordering.
//
// ORDER BY is chosen at query time, not at generate time, so its text is built
// from a table the back end fills here rather than baked into a constant. Two
// queries differing only in ordering are different statements.

// OrderLead and OrderSep punctuate the clause.
const (
	OrderLead = " ORDER BY "
	OrderSep  = ", "
)

// Directions, in the order runtime's constants number them.
const (
	dirAsc = iota
	dirDesc
	dirAscNullsFirst
	dirDescNullsLast
)

// OrderTerm lowers one ordering term.
//
// Defaults are not spelled out. ASC is Postgres's default direction, and NULLs
// go last for ASC and first for DESC, so writing either again changes nothing
// and makes the statement harder to read against a hand-written one. The other
// two NULL combinations ARE spelled out: they are what a keyset paginator
// needs, and leaving them implicit would silently change the order.
//
// This is not only cosmetic. bench asserts that generated SQL is byte-identical
// to the hand-written M0 spike, so that the two are compared on one query plan
// and not two — and a stray "ASC" is enough to break that.
func OrderTerm(dir int, ident string) string {
	switch dir {
	case dirDesc:
		return ident + " DESC"
	case dirAscNullsFirst:
		return ident + " ASC NULLS FIRST"
	case dirDescNullsLast:
		return ident + " DESC NULLS LAST"
	default:
		return ident
	}
}

// NDirections is how many orderings OrderTerm distinguishes.
const NDirections = 4

// LimitOffsetSuffix is the tail of a paged read. Both take placeholders the
// runtime numbers.
//
// OFFSET is generated because callers expect it, not because it is a good idea:
// the database still walks and discards every skipped row, so page 5,000 costs
// 5,000 pages of work. Keyset pagination is the answer, and `storm lint` is
// where a large constant offset should get flagged.
func LimitOffsetSuffix(withOffset bool) string {
	s := " LIMIT " + Placeholder
	if withOffset {
		s += " OFFSET " + Placeholder
	}
	return s
}

// Row comparison — what keyset pagination filters with.
//
// `(a, b) > (?1, ?2)` rather than the OR-expansion
// `a > ?1 OR (a = ?1 AND b > ?2)` that hand-written pagination usually reaches
// for. The two mean the same thing; only the row comparison lets Postgres walk
// a multi-column index once instead of planning a disjunction. It is also the
// first thing M9 will have to look at again — MySQL supports row comparison,
// SQL Server does not, and there the expansion is the only option.
const (
	TupleOpen  = "("
	TupleSep   = ", "
	TupleClose = ")"
)

// Row-comparison operators, in the order runtime numbers them.
const (
	cmpGt = iota
	cmpLt
)

// RowCmpOp lowers a row-comparison operator.
//
// Only strict inequality. A keyset paginator that used >= would return the row
// it just showed you, and one that used = has no ordering to follow.
func RowCmpOp(op int) string {
	if op == cmpLt {
		return " < "
	}
	return " > "
}

// Relation existence: the semi-join.
//
// `EXISTS (SELECT 1 FROM child WHERE child.fk = parent.pk)` is the correlated
// form of "has any children", and its negation is "has none". Both lower to
// CONSTANT fragments — no bound value, no placeholder — which is what lets
// them ride the ordinary predicate machinery and compose under AND/OR/NOT for
// free.
//
// The inner table is ALWAYS aliased. A self-referential relation correlates a
// table with itself ("orgs that have child orgs"), and without the alias the
// inner reference captures the outer table and the predicate silently means
// something else. Aliasing only for self-references would make the emitted
// SQL depend on a distinction the reader has to reconstruct; aliasing always
// costs nothing and reads the same everywhere.
const existsAlias = "_storm_e"

// ExistsFrag lowers "a related row exists". Negated by wrapping in NOT at the
// token level, or by NotExistsFrag for the direct spelling.
func ExistsFrag(childTable, childFK, parentTable, parentPK string, childLive Live) string {
	// childLive is the CHILD's predicate, under the inner alias: "this parent
	// has a related row" must not be satisfied by a row the child's own
	// package would refuse to return.
	return "EXISTS (SELECT 1 FROM " + Ident(childTable) + " AS " + Ident(existsAlias) +
		" WHERE " + Ident(existsAlias) + "." + Ident(childFK) +
		" = " + Ident(parentTable) + "." + Ident(parentPK) +
		aliasLive(childLive) + ")"
}

// aliasLive re-qualifies a child predicate to the exists alias and renders it
// as a trailing AND, or "" when the child does not soft-delete.
func aliasLive(l Live) string {
	if l.Empty() {
		return ""
	}
	return " AND " + string(LiveFor(existsAlias, liveCol(l)))
}

// NotExistsFrag lowers "no related row exists".
func NotExistsFrag(childTable, childFK, parentTable, parentPK string, childLive Live) string {
	return "NOT " + ExistsFrag(childTable, childFK, parentTable, parentPK, childLive)
}

// ExistsOpen is ExistsFrag WITHOUT its closing paren: the splicer appends the
// wrapped child predicates and closes. Split here rather than string-surgered
// in codegen, so the two forms cannot drift.
func ExistsOpen(childTable, childFK, parentTable, parentPK string, childLive Live) string {
	f := ExistsFrag(childTable, childFK, parentTable, parentPK, childLive)
	return f[:len(f)-1]
}

// ExistsOnOpen is ExistsOpen correlated on several column pairs rather than a
// key: the child row whose childCols equal the parent's parentCols, pair by
// pair, and is live.
func ExistsOnOpen(childTable string, childCols []string, parentTable string, parentCols []string, childLive Live) string {
	s := "EXISTS (SELECT 1 FROM " + Ident(childTable) + " AS " + Ident(existsAlias) + " WHERE "
	for i, c := range childCols {
		if i > 0 {
			s += " AND "
		}
		s += Ident(existsAlias) + "." + Ident(c) + " = " + Ident(parentTable) + "." + Ident(parentCols[i])
	}
	return s + aliasLive(childLive)
}

// NotExistsOnOpen is ExistsOnOpen negated.
func NotExistsOnOpen(childTable string, childCols []string, parentTable string, parentCols []string, childLive Live) string {
	return "NOT " + ExistsOnOpen(childTable, childCols, parentTable, parentCols, childLive)
}

// NotExistsOpen is ExistsOpen negated — the anti-join's header.
//
// `NOT EXISTS (...)` rather than `NOT (EXISTS (...))`: the same plan either
// way, and the shorter form is what a reviewer of the generated SQL expects to
// read.
func NotExistsOpen(childTable, childFK, parentTable, parentPK string, childLive Live) string {
	return "NOT " + ExistsOpen(childTable, childFK, parentTable, parentPK, childLive)
}
