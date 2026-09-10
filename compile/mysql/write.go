package mysql

import (
	"errors"
	"strings"
)

// ErrNoReturning is why a MySQL insert cannot learn what the server computed.
//
// This is the one MySQL difference that is not a spelling. compile/pgsql's own
// note on ReturningClause says it plainly: RETURNING "is not an optimisation
// here, it is the only correct way to learn a generated id or a DEFAULT the
// database computed. Reading them back with a second SELECT races every other
// writer."
//
// MySQL 8 has no RETURNING — measured, Error 1064 on 8.4.11 — and the usual
// substitutes do not close the gap:
//
//   - LAST_INSERT_ID() reports an AUTO_INCREMENT value and nothing else. It
//     cannot report a uuid DEFAULT, a computed timestamp, or a generated
//     column, which is most of what storm.Model asks the server for.
//   - A following SELECT is the race the PostgreSQL note refuses.
//
// So this is a MODEL constraint on a MySQL target, not a lowering to write: a
// model whose inserts need server-computed values back cannot be served
// correctly, and storm should say so at generate time rather than emit a write
// path that is quietly wrong under concurrency. MariaDB does have RETURNING and
// would not need this.
var ErrNoReturning = errors.New(
	"compile/mysql: MySQL 8 cannot return the row it wrote, and the values storm would " +
		"ask for — a uuid default, a server timestamp, a generated column — cannot be " +
		"recovered by LAST_INSERT_ID() or read back without racing another writer.\n" +
		"       Give the model explicit values for the columns the insert must know, or " +
		"target MariaDB, which has the clause.")

// InsertStmt is the whole INSERT, placeholders included.
//
// It REFUSES a non-empty returning list rather than dropping it. Dropping it
// would compile, run, and hand back a zero id — the shape of wrong answer this
// codebase exists not to produce.
func InsertStmt(table string, cols []string, returning []string) (string, error) {
	if len(returning) > 0 {
		return "", ErrNoReturning
	}
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
		// Bare, and therefore identical for every column: position is what
		// binds a value here, not a number.
		b.WriteString(Placeholder)
	}
	b.WriteString(")")
	return b.String(), nil
}

// InsertParts punctuates a masked insert, whose column list is not known until
// run time. Identical to PostgreSQL's because this part of INSERT is standard.
func InsertParts() (open, sep, mid, close string) {
	return " (", ", ", ") VALUES (", ")"
}

// Row locking.
//
// MySQL 8 has the whole surface storm generates — FOR UPDATE, FOR SHARE, and
// both NOWAIT and SKIP LOCKED on each — measured on 8.4.11. The clause goes at
// the very end, after LIMIT, exactly as it does on PostgreSQL, so the splicer
// appends it without knowing anything about it.
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

// LockSuffix is the clause.
func LockSuffix(m int) string {
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
