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

// AddFrag adds a bound amount to a counter's own value, in the database. Two
// writers who both add land both amounts, which a value computed in Go and
// written back would not.
func AddFrag(col string) (a, b string) {
	return Ident(col) + " = " + Ident(col) + " + " + Placeholder, ""
}

// IncWhenFrag adds one to a counter when another column, as the row stands
// before the write, equals a bound value, and nothing otherwise. The test is
// read in the statement that writes, so it sees the value the write replaces.
func IncWhenFrag(col, cond string) (a, b string) {
	return Ident(col) + " = " + Ident(col) + " + CASE WHEN " + Ident(cond) + " = " + Placeholder, " THEN 1 ELSE 0 END"
}

// TakeOpen narrows an UPDATE to the rows a read selects: the read's
// predicate, order and limit run inside the subquery, so the UPDATE writes
// exactly the rows it would have returned. There is no row lock to take inside
// it, and none is needed: the statement is one write, and SQLite runs one
// writer at a time.
func TakeOpen(table, key string) string {
	return " WHERE " + Ident(key) + " IN (SELECT " + Ident(key) + " FROM " + Ident(table)
}

// TakeClose ends what TakeOpen began.
const TakeClose = ")"

// TrimOpen narrows a DELETE to the rows a read selects, by rowid: SQLite's
// physical address, which every table storm creates has. PostgreSQL's form
// uses ctid for the same reason — the read runs once and the delete is a
// lookup per row it found, not a second scan.
func TrimOpen(table string) string {
	return " WHERE rowid IN (SELECT rowid FROM " + Ident(table)
}

// TrimClose ends what TrimOpen began.
const TrimClose = ")"

// InsertSelectMid is what an INSERT … SELECT has between its column list and
// the selected values, where a plain insert has VALUES.
const InsertSelectMid = ") SELECT "

// KeyFrom selects one table's key, the tail of an INSERT … SELECT that fills a
// reference from a read of that table.
func KeyFrom(table, key string) string { return Ident(key) + " FROM " + Ident(table) }

// Now is the database's clock in the one shape every timestamp is stored in
// (see compile/liteddl): UTC, nine fractional digits. strftime's %f is
// milliseconds, so the fraction is padded rather than left three wide — text
// order is time order only when every value has the same width.
//
// It is the clock of the STATEMENT, not of the transaction as PostgreSQL's
// now() is. SQLite fixes 'now' once per statement (sqlite3_step), so every row
// one statement stamps agrees; two statements in one transaction may not.
const Now = "strftime('%Y-%m-%dT%H:%M:%f000000Z', 'now')"

// NowFrag assigns a column the DATABASE's clock rather than the caller's: a
// client that binds its own clock lets two application servers that skew
// disagree about the order things happened in.
func NowFrag(col string) (a, b string) { return Ident(col) + " = " + Now, "" }

// DeletePrefix introduces a delete.
func DeletePrefix(table string) string { return "DELETE FROM " + Ident(table) }

// Soft delete lowering. A soft delete is an UPDATE that sets the mark, and a
// restore is the UPDATE that clears it; neither takes a bound value, because
// the timestamp is the server's clock and NULL is a literal. A client-supplied
// "now" would let two rows deleted in the same request disagree about when,
// and would make the mark a value a caller could choose.

// SoftDeleteSet marks a row deleted, stamped by the server.
func SoftDeleteSet(table, col string) string {
	return "UPDATE " + Ident(table) + " SET " + Ident(col) + " = " + Now
}

// RestoreSet clears the mark, bringing a row back.
func RestoreSet(table, col string) string {
	return "UPDATE " + Ident(table) + " SET " + Ident(col) + " = NULL"
}

// The predicates that separate the live rows from the marked ones are not new
// lowerings: "IsNull" and "IsNotNull" are already in the operator table, so a
// soft delete asks for them the same way any other predicate does.

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

// Upsert lowering.
//
// The conflict target is a *column list*, never a constraint name. A constraint
// name is a database artifact the model does not control and a migration can
// rename; the columns are in the model, so a target that stops existing is a
// generation error rather than a runtime one.
//
// SQLite's upsert is PostgreSQL's — ON CONFLICT with a target, DO NOTHING or
// DO UPDATE, the rejected row named EXCLUDED — so this is the one back end
// whose generated upsert methods keep PostgreSQL's names and meanings exactly.

// ConflictSpec renders the inference specification: the keys of the unique
// index a conflict is expected on, and — for a PARTIAL index — its predicate.
//
// The predicate is not decoration: SQLite, like PostgreSQL, matches a partial
// unique index only when the target repeats its WHERE, and without it the
// insert fails on the first row that conflicts.
func ConflictSpec(keys []ConflictKey, where string) string {
	var b strings.Builder
	b.WriteString(" ON CONFLICT (")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		if k.Expr {
			b.WriteString("(" + k.Name + ")")
		} else {
			b.WriteString(Ident(k.Name))
		}
		// A collation is part of what identifies the index. There are no
		// operator classes here; compile/liteddl refuses an index with one.
		if k.Collate != "" {
			b.WriteString(" COLLATE " + collation(k.Collate))
		}
	}
	b.WriteString(")")
	if where != "" {
		b.WriteString(" WHERE " + where)
	}
	return b.String()
}

// ConflictKey is one key of an inference specification.
type ConflictKey struct {
	Name    string
	Expr    bool
	Collate string
	OpClass string
}

// ConflictAny is "any unique constraint" — legal only with DO NOTHING, which
// is what makes it useful: an insert that is a no-op if the row is already
// there, whichever key it collides on.
const ConflictAny = " ON CONFLICT"

// ConflictDoNothing means "insert if absent, otherwise leave it alone".
const ConflictDoNothing = " DO NOTHING"

// ConflictDoUpdate introduces the assignment list.
const ConflictDoUpdate = " DO UPDATE SET "

// ExcludedAssign assigns one column from the rejected row.
func ExcludedAssign(col string) string {
	return Ident(col) + " = EXCLUDED." + Ident(col)
}

// ExcludedAdd accumulates into one column instead of overwriting it: the row
// already there keeps its value plus the rejected row's.
//
// The existing value is qualified by the table name, as in PostgreSQL, so the
// right-hand side cannot be read as anything but the target row.
func ExcludedAdd(table, col string) string {
	return Ident(col) + " = " + Ident(table) + "." + Ident(col) + " + EXCLUDED." + Ident(col)
}

// ConflictAssignSep joins assignments.
const ConflictAssignSep = ", "

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

// collation maps a collation name to SQLite's three built-in ones, the same
// mapping compile/liteddl applies when it creates the index.
func collation(name string) string {
	switch strings.ToUpper(strings.Trim(name, `"`)) {
	case "C", "POSIX", "BINARY":
		return "BINARY"
	case "NOCASE":
		return "NOCASE"
	case "RTRIM":
		return "RTRIM"
	}
	return Ident(name)
}

// Row locking.
//
// SQLite has no row locks. It has one writer at a time, and the lock that
// makes it so is the DATABASE's: a transaction begun IMMEDIATE holds it from
// its first statement to COMMIT, so no other connection can change any row it
// reads. Every lock mode a generated Query offers is therefore already in
// force inside such a transaction, and each lowers to nothing:
//
//   - ForUpdate, ForShare and ForNoKeyUpdate: no other writer can touch the
//     row until COMMIT, which is the promise all three make.
//   - ForUpdateSkipLocked: no row can be locked by anyone else while this
//     transaction holds the write lock, so skipping the locked rows skips
//     none — the queue claim returns what a claim by the only worker would.
//   - The NoWait forms: the wait happens at BEGIN, bounded by busy_timeout,
//     not at the read. A NoWait read inside a transaction never waits.
//
// The condition is IMMEDIATE, and runtime/sqlitedrv's DSN sets it: a deferred
// BEGIN takes no lock until its first write, and a read before then locks
// nothing. That is said in the generated doc comment, where a caller reads it.

// LockMode is a row-level lock, numbered as every back end numbers it: the
// mode is an index into a generated statement-cache array.
type LockMode uint8

// The lock modes, in the order the generated cache array is indexed.
const (
	LockNone LockMode = iota
	LockUpdate
	LockUpdateNoWait
	LockUpdateSkipLocked
	LockShare
	LockShareNoWait
	LockShareSkipLocked
	LockNoKeyUpdate
	numLockModes
)

// NumLockModes is how many lock states a generated Query can be in, including
// none — the width of its statement-cache array.
const NumLockModes = int(numLockModes)

// LockSuffix is empty for every mode: see Row locking above.
func LockSuffix(int) string { return "" }

// LockNotes is what a caller has to know before locking anything on this back
// end, for the generated doc comment.
func LockNotes() []string {
	return []string{
		"SQLite has no row locks: it runs one writer at a time, and every lock",
		"mode here is the DATABASE's write lock, held from BEGIN to COMMIT. So",
		"each one adds nothing to the statement and holds only inside a",
		"transaction begun IMMEDIATE — which runtime/sqlitedrv's DSN asks for.",
		"Under a deferred BEGIN a read takes no lock at all.",
		"",
		"Inside such a transaction no other connection can change a row this",
		"one read, SkipLocked skips nothing (nothing else can hold a lock), and",
		"NoWait never waits at the read: the wait is at BEGIN, bounded by",
		"busy_timeout.",
	}
}
