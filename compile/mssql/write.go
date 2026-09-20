package mssql

import "strings"

// The write path, and the one structural difference that shapes it: OUTPUT is
// POSITIONAL.
//
// PostgreSQL's RETURNING and MariaDB's both go at the end of the statement, so
// two dialects were served by handing the splicer a suffix. SQL Server's clause
// sits between the assignments and the predicate:
//
//	INSERT INTO t (a, b) OUTPUT INSERTED.a, INSERTED.b VALUES (@p1, @p2)
//	UPDATE t SET a = @p1 OUTPUT INSERTED.a WHERE id = @p2
//
// At the end it is a syntax error, not a slower plan. So the insert carries it
// in InsertParts.Mid — the punctuation between the column list and VALUES,
// which is exactly where it goes — and the update hands it to
// runtime.SpliceSectionsOutput, which writes it before the last section.
// ReturningPositional is how the seam asks for both.

// ReturningPositional says this back end's returning clause is not a suffix.
const ReturningPositional = true

// ReturningClause names the columns an insert or update hands back.
//
// INSERTED is the post-image pseudo-table, the one storm wants: it carries the
// uuid a DEFAULT generated, the timestamp the server stamped and the value a
// computed column derived, which is the whole reason the clause is used.
//
// Empty list, empty clause — a write that asks for nothing back gets no clause
// rather than an OUTPUT with no columns, which does not parse.
func ReturningClause(cols []string) string {
	if len(cols) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(" OUTPUT ")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("INSERTED.")
		b.WriteString(Ident(c))
	}
	return b.String()
}

// InsertStmt is the whole INSERT, placeholders included.
//
// Named parameters, so they are numbered here at generate time rather than
// repeated — the text is fixed for this column set and the ordinals with it.
func InsertStmt(table string, cols []string, returning []string) (string, error) {
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
	b.WriteString(")")
	b.WriteString(ReturningClause(returning))
	b.WriteString(" VALUES (")
	for i := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(Param(i + 1))
	}
	b.WriteString(")")
	return b.String(), nil
}

// InsertParts punctuates a masked insert, whose column list is not known until
// run time.
//
// Mid carries the OUTPUT clause, because Mid IS the text between the column
// list and VALUES and that is where the clause belongs. The returning list is
// fixed at generate time — it is the table's readable columns — so there is
// nothing dynamic about it; only the column list being inserted varies.
func InsertParts(returning []string) (open, sep, mid, close string) {
	return " (", ", ", ")" + ReturningClause(returning) + " VALUES (", ")"
}

// Row locking, which is a TABLE HINT here rather than a clause.
//
// `FOR UPDATE` does not exist in T-SQL. The equivalent is a hint attached to
// the table reference in FROM:
//
//	SELECT ... FROM t WITH (UPDLOCK, ROWLOCK) WHERE ...
//
// So it goes in the statement's PREFIX, not its suffix — LockHint rather than
// LockSuffix — and codegen appends it to the select prefix for this back end.
// ROWLOCK is named explicitly because without it the server is free to take a
// page or table lock instead, and a row lock is what the caller asked for.
//
// The modes are numbered by pgsql and that numbering is the generated Query's,
// not a back end's to choose: a mode is an index into a statement-cache array.
const (
	LockNone = iota
	LockUpdate
	LockUpdateNoWait
	LockUpdateSkipLocked
	LockShare
	LockShareNoWait
	LockShareSkipLocked
	numLockModes
)

// NumLockModes is how many lock states a generated Query can be in.
const NumLockModes = int(numLockModes)

// LockSuffix is empty for every mode: nothing goes at the end. It exists
// because the seam has the shape of a suffix and the honest answer for this
// back end is "none", not "unsupported" — LockHint is where the lock is.
func LockSuffix(int) string { return "" }

// LockHint is the table hint, written directly after the table name.
//
// The share modes are REPEATABLEREAD rather than HOLDLOCK. Both hold the shared
// lock to the end of the transaction, which is what FOR SHARE means; HOLDLOCK
// is SERIALIZABLE, which additionally takes RANGE locks and so refuses inserts
// into the gaps the query read. That is a stronger promise than the caller
// asked for and a much bigger blocking surface.
func LockHint(m int) string {
	switch m {
	case LockUpdate:
		return " WITH (UPDLOCK, ROWLOCK)"
	case LockUpdateNoWait:
		return " WITH (UPDLOCK, ROWLOCK, NOWAIT)"
	case LockUpdateSkipLocked:
		// READPAST is SKIP LOCKED: rows another transaction has locked are
		// passed over rather than waited for. It is only honoured for row-level
		// locks, which is the other reason ROWLOCK is spelled out.
		return " WITH (UPDLOCK, ROWLOCK, READPAST)"
	case LockShare:
		return " WITH (REPEATABLEREAD, ROWLOCK)"
	case LockShareNoWait:
		return " WITH (REPEATABLEREAD, ROWLOCK, NOWAIT)"
	case LockShareSkipLocked:
		return " WITH (REPEATABLEREAD, ROWLOCK, READPAST)"
	}
	return ""
}

// LockNotes is what a generated package's doc comment says about locking here.
//
// It lives in this package rather than in codegen for the reason R9 exists: the
// text names KEYWORDS, and a keyword in codegen is a keyword one dialect owns
// leaking into every other one's output.
func LockNotes() []string {
	return []string{
		"A lock is held to the end of the TRANSACTION, so one taken outside a",
		"transaction is released before the next statement runs and protects",
		"nothing — pass an msdrv.Tx, not a pool.",
		"",
		"On this target the lock is a TABLE HINT written after the table name,",
		"not a trailing clause: T-SQL has no FOR UPDATE. Two of the modes are",
		"not quite what their names suggest. Skip-locked is READPAST, which is",
		"honoured only for row-level locks — which is why ROWLOCK is always",
		"spelled out. A shared lock is REPEATABLEREAD rather than HOLDLOCK:",
		"HOLDLOCK is SERIALIZABLE and would take range locks, refusing inserts",
		"into the gaps this query read, which is a stronger promise than the",
		"caller asked for.",
		"",
		"Locking is refused on Count and Exists, and on the declared",
		"aggregations and joins, for the reason it is refused everywhere else:",
		"a lock combined with an aggregate or a set operation locks something",
		"other than the rows the caller is looking at.",
	}
}

// Soft delete. The predicate and the two marks, in the only package allowed to
// write SQL text for this back end — codegen may not spell them itself (R9).

// SoftDeleteWhere keeps marked rows out of a read.
func SoftDeleteWhere(col string) string { return Ident(col) + " IS NULL" }

// SoftDeleteSet marks a row deleted, stamped by the server. See NowFrag for why
// it is SYSDATETIMEOFFSET() and not GETDATE().
func SoftDeleteSet(table, col string) string {
	return UpdatePrefix(table) + Ident(col) + " = SYSDATETIMEOFFSET()"
}

// RestoreSet clears the mark.
func RestoreSet(table, col string) string {
	return UpdatePrefix(table) + Ident(col) + " = NULL"
}

// LiveFor is the predicate for a table, optionally qualified by the alias it is
// read under. Empty col means the table does not soft-delete.
func LiveFor(alias, col string) string {
	if col == "" {
		return ""
	}
	if alias == "" {
		return SoftDeleteWhere(col)
	}
	return Ident(alias) + "." + SoftDeleteWhere(col)
}

// Param is a parameter NAME, numbered.
//
// The statements that need it are the ones whose text is FIXED at generate
// time — a top-N loader, a recursive traversal, a union's row cap. Those never
// reach the splicer, so nothing else will number them: PostgreSQL's equivalents
// write `$1` and `$2` for exactly this reason, and the first version of this
// package left the bare sigil there. Every one of those statements came back
// `Must declare the scalar variable "@"` the first time a server saw it.
func Param(n int) string { return Placeholder + "p" + itoa(n) }

// itoa is the small-integer formatter, so this package does not import strconv
// for the handful of ordinals an INSERT spells at generate time.
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
