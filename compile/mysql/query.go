// Package mysql lowers query structure to MySQL text.
//
// It is the second implementation of the seam's QUERY side. Until it existed
// there was one — compile/pgsql — and it served both dialects, so a
// MySQL-dialect package came out carrying PostgreSQL SQL that MySQL 8.4.11
// rejects with Error 1064. See docs/PLAN.md M9.
//
// What differs from PostgreSQL, and why each is a real difference rather than
// a spelling:
//
//   - Identifiers are backticked. MySQL's default sql_mode has no ANSI_QUOTES,
//     so a double-quoted name is a string LITERAL. `SELECT "id" FROM "users"`
//     is not a failing query; it is a query that selects the constant "id".
//   - Placeholders are a bare `?`. Position binds a value, not a number, so a
//     lowering that reuses an ordinal has to bind the value twice rather than
//     write the same marker twice (ADR-0010, runtime.Placeholder).
//   - There is no output clause on INSERT. MySQL 8 cannot return the row it
//     wrote; the generated write path has to read it back.
//
// This package covers the BASE read and write path. Joins, aggregates, unions,
// top-N, recursion and upsert are still PostgreSQL-only, and codegen refuses
// them for this dialect rather than emitting the other dialect's SQL.
//
// Output is byte-deterministic: same input, same bytes, always.
package mysql

import "strings"

// Ident quotes an identifier.
//
// Backticks, and a backtick inside one is doubled. MySQL will accept a
// double-quoted identifier ONLY under ANSI_QUOTES, which is not the default and
// is not something a library may assume about someone else's server.
func Ident(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// Placeholder marks where a bound parameter goes. Bare: MySQL binds by
// position, so nothing follows it. runtime.Placeholder carries this to the
// splicer, which knows not to number it.
const Placeholder = "?"

// SelectPrefix is everything before the WHERE clause of a row read.
func SelectPrefix(table string, cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = Ident(c)
	}
	return "SELECT " + strings.Join(q, ", ") + " FROM " + Ident(table)
}

// CountPrefix is everything before the WHERE clause of a count.
func CountPrefix(table string) string { return "SELECT count(*) FROM " + Ident(table) }

// ExistsPrefix projects nothing, for the same reason it does on PostgreSQL: an
// existence probe that selects real columns pays decoding for a boolean.
func ExistsPrefix(table string) string { return "SELECT 1 FROM " + Ident(table) }

// ExistsSuffix caps an existence probe.
func ExistsSuffix() string { return " LIMIT 1" }

// OrderSuffix is the ordering and the row cap.
func OrderSuffix(orderBy string) string {
	return " ORDER BY " + orderBy + " LIMIT " + Placeholder
}

// LimitOffsetSuffix is the tail of a paged read.
//
// MySQL spells it LIMIT n OFFSET m, the same way PostgreSQL does — the older
// `LIMIT m, n` reverses the operands and is not worth the ambiguity.
func LimitOffsetSuffix(withOffset bool) string {
	s := " LIMIT " + Placeholder
	if withOffset {
		s += " OFFSET " + Placeholder
	}
	return s
}

// DefaultOrderBy is the ordering used when the caller names none: the primary
// key, because a read without ORDER BY has no defined order and paging one is a
// bug waiting for a plan change.
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

// OrderLead and OrderSep punctuate an ORDER BY built at query time.
const (
	OrderLead = " ORDER BY "
	OrderSep  = ", "
)

// Order directions, in the order runtime numbers them. Mirrors pgsql's, because
// the numbering is the token stream's and not a back end's to choose.
const (
	dirAsc = iota
	dirDesc
	dirAscNullsFirst
	dirDescNullsLast
)

// NDirections is how many orderings OrderTerm distinguishes.
const NDirections = 4

// OrderTerm lowers one ORDER BY term.
//
// MySQL has no NULLS FIRST / NULLS LAST — measured: Error 1064 on 8.4.11. It
// sorts NULLs first ascending and last descending, and the only way to ask for
// the other placement is a leading sort key that says whether the value is
// null. `ISNULL(x)` is 0 for a value and 1 for NULL, so `ISNULL(x), x ASC` puts
// the nulls last.
//
// That extra key is not free: it is an expression, so a plain index on x no
// longer satisfies the ordering and the server sorts. PostgreSQL attaches the
// placement to the index instead. A model that asks for NULLS LAST on a MySQL
// target is therefore asking for a sort, and `storm lint` is where that should
// be surfaced — but it is CORRECT, which the alternative of silently dropping
// the placement would not be.
func OrderTerm(dir int, ident string) string {
	switch dir {
	case dirDesc:
		return ident + " DESC"
	case dirAscNullsFirst:
		// Ascending already puts NULLs first on MySQL; saying so costs a sort
		// for nothing, so this is the plain form on purpose.
		return ident
	case dirDescNullsLast:
		// Descending already puts NULLs last on MySQL. Same reasoning.
		return ident + " DESC"
	default:
		return ident
	}
}

// Row comparison — what keyset pagination filters with.
//
// `(a, b) > (?, ?)` rather than the OR-expansion. MySQL supports it, measured
// on 8.4.11, so keyset pagination crosses unchanged. SQL Server does not, and
// M10 will have to expand it there.
const (
	TupleOpen  = "("
	TupleSep   = ", "
	TupleClose = ")"
)

const (
	cmpGt = iota
	cmpLt
)

// RowCmpOp lowers a row-comparison operator. Strict inequality only, for the
// same reason as PostgreSQL: >= returns the row you just showed.
func RowCmpOp(op int) string {
	if op == cmpLt {
		return " < "
	}
	return " > "
}

// Section punctuation, identical to PostgreSQL's because SQL's is.
const (
	SetLead   = ""
	SetSep    = ", "
	WhereLead = " WHERE "
	WhereSep  = " AND "
)

// UpdatePrefix introduces the SET list.
func UpdatePrefix(table string) string { return "UPDATE " + Ident(table) + " SET " }

// DeletePrefix introduces a delete.
func DeletePrefix(table string) string { return "DELETE FROM " + Ident(table) }

// SetFrag assigns one column from a bound value.
func SetFrag(col string) (a, b string) { return Ident(col) + " = " + Placeholder, "" }

// BumpFrag increments a version column from its own value.
func BumpFrag(col string) (a, b string) { return Ident(col) + " = " + Ident(col) + " + 1", "" }

// InsertPrefix introduces a masked insert.
func InsertPrefix(table string) string { return "INSERT INTO " + Ident(table) }
