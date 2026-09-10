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

// LimitOffsetSuffix is the paged form.
//
// MySQL spells it LIMIT n OFFSET m, the same way PostgreSQL does — the older
// `LIMIT m, n` reverses the operands and is not worth the ambiguity.
func LimitOffsetSuffix(orderBy string) string {
	return " ORDER BY " + orderBy + " LIMIT " + Placeholder + " OFFSET " + Placeholder
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

// OrderTerm renders one ORDER BY term.
func OrderTerm(ident string, desc bool) string {
	if desc {
		return ident + " DESC"
	}
	return ident
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
