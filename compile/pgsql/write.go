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

// Soft delete lowering. A soft delete is an UPDATE that sets the mark, and a
// restore is the UPDATE that clears it; neither takes a bound value, because
// the timestamp is the server's clock and NULL is a literal. A client-supplied
// "now" would let two rows deleted in the same request disagree about when,
// and would make the mark a value a caller could choose.

// SoftDeleteSet marks a row deleted, stamped by the server.
func SoftDeleteSet(table, col string) string {
	return "UPDATE " + Ident(table) + " SET " + Ident(col) + " = now()"
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
// EXCLUDED is how Postgres names the row the INSERT tried to write. Every
// target dialect spells this differently — MySQL has ON DUPLICATE KEY UPDATE
// with VALUES(), MSSQL has MERGE with a different shape entirely — which is
// exactly why it lives here and not in codegen.

// ConflictSpec renders the inference specification: the keys of the unique
// index a conflict is expected on, and — for a PARTIAL index — its predicate.
//
// The predicate is not decoration. PostgreSQL infers the index from the keys
// AND the predicate together, and omitting it on a partial index is
// SQLSTATE 42P10, "there is no unique or exclusion constraint matching the ON
// CONFLICT specification" — at run time, on the first row that conflicts,
// which is a code path a test that inserts distinct rows never reaches.
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
		// A collation or an operator class is part of what identifies the
		// index, and two unique indexes over one column can differ by nothing
		// else. Emitted so the inference cannot be ambiguous.
		if k.Collate != "" {
			b.WriteString(" COLLATE " + Ident(k.Collate))
		}
		if k.OpClass != "" {
			b.WriteString(" " + k.OpClass)
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

// Row locking.

// LockMode is a row-level lock: a strength, and what to do when the row is
// already locked. The zero value locks nothing.
//
// Two strengths rather than PostgreSQL's four. FOR NO KEY UPDATE and FOR KEY
// SHARE exist for the deadlock between a parent update and a child insert,
// which is real and rare, and every caller who has it knows the exact SQL
// they want — so they get storm.SQL rather than four more methods on every
// generated table.
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
	numLockModes
)

// NumLockModes is how many lock states a generated Query can be in, including
// none — the width of its statement-cache array.
const NumLockModes = int(numLockModes)

// LockName is the Go method that selects this mode. The name is the API's,
// but it is derived from the clause, so the two live together and a mode
// added here cannot be forgotten there.
func LockName(m LockMode) string {
	switch m {
	case LockUpdate:
		return "ForUpdate"
	case LockUpdateNoWait:
		return "ForUpdateNoWait"
	case LockUpdateSkipLocked:
		return "ForUpdateSkipLocked"
	case LockShare:
		return "ForShare"
	case LockShareNoWait:
		return "ForShareNoWait"
	case LockShareSkipLocked:
		return "ForShareSkipLocked"
	}
	return ""
}

// LockDoc is what the mode does, for the generated method's doc comment.
// It lives here because it describes THIS back end's locking, and the
// generator is not allowed to know any.
func LockDoc(m LockMode) string {
	switch m {
	case LockUpdate:
		return "takes the strongest row lock, waiting for anyone who already holds it."
	case LockUpdateNoWait:
		return "fails immediately rather than wait for a lock someone else holds."
	case LockUpdateSkipLocked:
		return "steps over the rows another transaction has locked. This is the " +
			"queue claim, and the one form that returns FEWER rows than Limit " +
			"asks for: that is the point of it, not a fault."
	case LockShare:
		return "blocks writers while letting other readers share the lock."
	case LockShareNoWait:
		return "is the shared lock, failing rather than waiting."
	case LockShareSkipLocked:
		return "is the shared lock, stepping over rows another transaction holds."
	}
	return ""
}

// LockNotes is what a caller has to know before locking anything on this back
// end, for the generated doc comment.
func LockNotes() []string {
	return []string{
		"A lock is held to the end of the TRANSACTION, so one taken outside a",
		"transaction is released before the next statement runs and protects",
		"nothing — pass a pgxdrv.Tx, not a pool.",
		"",
		"Locking is refused on Count and Exists, and on the declared",
		"aggregations and joins. The server rejects a row lock combined with an",
		"aggregate, a grouping, a DISTINCT or a set operation, and on the",
		"nullable side of an outer join; refusing at the call site names the",
		"rule, where the server would name a SQLSTATE.",
		"",
		"The two weakest strengths are deliberately absent. They exist for the",
		"deadlock between updating a parent row and inserting a child that",
		"references it, which is real and rare, and a caller who has it knows",
		"the exact SQL they want — storm.SQL gives it to them typed.",
	}
}

// LockRefusedGrouped and LockRefusedJoined are why this back end will not
// take a row lock on a declared aggregation or join. They live here for the
// same reason LockDoc does: which shapes refuse a lock is the back end's
// rule, and the generator is not allowed to know one.
func LockRefusedGrouped() string {
	return "storm: a grouped read cannot be row-locked — the server refuses a row " +
		"lock with a GROUP BY, and locking the rows a group summarises is not " +
		"what the caller asked for; lock the base read instead"
}

// LockRefusedJoined is the same for a declared join.
func LockRefusedJoined() string {
	return "storm: a declared join cannot be row-locked — the server refuses a row " +
		"lock on the nullable side of an outer join, and which side a lock would " +
		"take is not something the call site says; lock the base read instead"
}

// LockRefusedCounted and LockRefusedProbed are the same for the two scalar
// terminals.
func LockRefusedCounted() string {
	return "storm: a locked read cannot be counted — the server refuses a row lock " +
		"with an aggregate; count first, then lock the rows you take"
}

// LockRefusedProbed is the existence probe's.
func LockRefusedProbed() string {
	return "storm: a locked read cannot be an existence probe — locking a row to " +
		"answer a boolean is a row nobody reads; use One() with the same lock"
}

// LockSuffix is the clause, which goes at the very END of the statement:
// after LIMIT and OFFSET, which is both what the grammar requires and what
// makes it a suffix the splicer can append without knowing anything about it.
func LockSuffix(m LockMode) string {
	switch m {
	case LockUpdate:
		return " FOR UPDATE"
	case LockUpdateNoWait:
		return " FOR UPDATE NOWAIT"
	case LockUpdateSkipLocked:
		return " FOR UPDATE SKIP LOCKED"
	case LockShare:
		return " FOR SHARE"
	case LockShareNoWait:
		return " FOR SHARE NOWAIT"
	case LockShareSkipLocked:
		return " FOR SHARE SKIP LOCKED"
	}
	return ""
}
