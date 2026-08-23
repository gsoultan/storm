package pgsql

import (
	"strconv"
	"strings"
)

// Write lowering. Everything here is the same bargain as the read path: the
// text is chosen at generate time, and the runtime only splices what varies.
//
// What varies, and what does not:
//   - INSERT does not vary. The column list is fixed by the table, so the whole
//     statement including its placeholders is a constant in generated code.
//   - UPDATE varies by which fields were assigned. That is a *shape* in exactly
//     the sense the read path uses the word — a dirty mask picks a set of SET
//     fragments, and the statement is compiled once per distinct mask.
//   - DELETE varies only by its predicate.

// InsertStmt is the whole INSERT, placeholders included, because none of it
// depends on the values.
func InsertStmt(table string, cols []string, returning []string) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(Ident(table))
	b.WriteString(" (")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(Ident(c))
	}
	b.WriteString(") VALUES (")
	for i := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(Placeholder)
		b.WriteString(strconv.Itoa(i + 1))
	}
	b.WriteString(")")
	b.WriteString(ReturningClause(returning))
	return b.String()
}

// ReturningClause is what the database sends back after a write. Empty for no
// columns, so callers can concatenate it unconditionally.
//
// RETURNING is not an optimisation here, it is the only correct way to learn a
// generated id or a DEFAULT the database computed. Reading them back with a
// second SELECT races every other writer.
func ReturningClause(cols []string) string {
	if len(cols) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(" RETURNING ")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(Ident(c))
	}
	return b.String()
}

// InsertPrefix introduces a masked insert, whose column list is not known
// until run time.
func InsertPrefix(table string) string { return "INSERT INTO " + Ident(table) }

// UpdatePrefix introduces the SET list.
func UpdatePrefix(table string) string { return "UPDATE " + Ident(table) + " SET " }

// SetFrag assigns one column from a bound value.
func SetFrag(col string) (a, b string) { return Ident(col) + " = " + Placeholder, "" }

// BumpFrag increments a version column from its own value rather than from one
// the client read. Two writers that both saw version 3 must not both write 4:
// the WHERE clause rejects the loser, and the increment never depends on what
// the winner had in memory.
func BumpFrag(col string) (a, b string) { return Ident(col) + " = " + Ident(col) + " + 1", "" }

// DeletePrefix introduces a delete.
func DeletePrefix(table string) string { return "DELETE FROM " + Ident(table) }

// Section punctuation. A SET list and a WHERE clause differ only in how they
// are introduced and joined, so both are Sections and these are the only
// strings that distinguish them.
const (
	SetLead   = ""
	SetSep    = ", "
	WhereLead = " WHERE "
	WhereSep  = " AND "
)

// CopyTarget names the table and columns for a bulk load. The protocol-level
// COPY is the driver's; this is only the target it is told to fill.
func CopyTarget(table string, cols []string) (string, []string) {
	out := make([]string, len(cols))
	copy(out, cols)
	return table, out
}

// InsertParts is the punctuation an INSERT needs when its column list is not
// known until run time.
//
// A masked insert cannot be precomputed — N columns have 2^N possible column
// lists — so the statement is assembled on the cold path from a column-name
// table. These are the only pieces of it that are SQL rather than identifiers,
// and they live here so no SQL text has to appear in codegen or runtime.
func InsertParts() (open, sep, mid, close string) {
	return " (", ", ", ") VALUES (", ")"
}
