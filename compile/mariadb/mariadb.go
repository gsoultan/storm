// Package mariadb lowers query structure to MariaDB text.
//
// MariaDB speaks the MySQL WIRE protocol, so it needs no driver of its own —
// but it is not MySQL at the SQL level, and treating it as an alias would emit
// four constructs it rejects. Measured against MariaDB 11.4.13 and MySQL
// 8.4.11, for the constructs storm actually emits:
//
//	                            MySQL 8   MariaDB 11.4
//	INSERT … RETURNING             no        YES
//	LATERAL                        yes        no
//	GROUPING()                     yes        no
//	WITH ROLLUP + ORDER BY         yes        no    (Error 1221)
//	FOR SHARE                      yes        no    (LOCK IN SHARE MODE)
//
// Everything else crosses: JSON_TABLE, WITH RECURSIVE, FIND_IN_SET, window
// functions, row comparison, FOR UPDATE with NOWAIT and SKIP LOCKED,
// JSON_CONTAINS. Neither has NULLS FIRST/LAST.
//
// So this package is deliberately thin: it holds the five differences and
// nothing else, and compile/mysql supplies the rest. A second full
// implementation would be four fifths duplicate, and the duplicate four fifths
// is where the drift would happen.
package mariadb

import (
	"errors"
	"strings"

	"github.com/gsoultan/storm/compile/mysql"
	"github.com/gsoultan/storm/schema"
)

// InsertStmt is the whole INSERT, and unlike MySQL's it can return the row it
// wrote.
//
// This is the difference that pays for the dialect. compile/pgsql's note is
// that RETURNING "is not an optimisation here, it is the only correct way to
// learn a generated id or a DEFAULT the database computed" — MariaDB has it, so
// an insert here keeps the semantics PostgreSQL has and MySQL cannot, and the
// generated Insert fills the caller's Row rather than leaving it untouched.
func InsertStmt(table string, cols []string, returning []string) (string, error) {
	base, err := mysql.InsertStmt(table, cols, nil)
	if err != nil {
		return "", err
	}
	return base + ReturningClause(returning), nil
}

// ReturningClause is what the database sends back after a write.
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
		b.WriteString(mysql.Ident(c))
	}
	return b.String()
}

// LockSuffix is the row-lock clause.
//
// MariaDB spells the shared lock LOCK IN SHARE MODE, which MySQL 8 also accepts
// but deprecated in favour of FOR SHARE — and MariaDB does not accept FOR SHARE
// at all. NOWAIT and SKIP LOCKED attach to it the same way.
func LockSuffix(m int) string {
	switch m {
	case mysql.LockShare:
		return " LOCK IN SHARE MODE"
	case mysql.LockShareNoWait:
		return " LOCK IN SHARE MODE NOWAIT"
	case mysql.LockShareSkipLocked:
		return " LOCK IN SHARE MODE SKIP LOCKED"
	}
	// The exclusive modes are spelled identically.
	return mysql.LockSuffix(m)
}

// ErrNoOrderedRollup is why a rolled-up aggregation cannot be lowered.
var ErrNoOrderedRollup = errors.New(
	"compile/mariadb: MariaDB rejects WITH ROLLUP together with ORDER BY (Error 1221), and " +
		"storm always orders a grouped read — an unordered GROUP BY makes a paginated report " +
		"shuffle between requests. Dropping the ordering would trade a refusal for a report " +
		"that pages wrongly, so the rollup is refused instead")

// GroupBy renders the grouping clause, refusing the rolled-up form.
func GroupBy(agg *schema.Aggregate) (string, error) {
	if agg.Sets != nil {
		return "", ErrNoOrderedRollup
	}
	return mysql.GroupBy(agg)
}

// AggregateSuffix is the GROUP BY, HAVING and ORDER BY.
func AggregateSuffix(agg *schema.Aggregate) (string, error) {
	if agg.Sets != nil {
		return "", ErrNoOrderedRollup
	}
	return mysql.AggregateSuffix(agg)
}

// ErrNoLateral is why the lateral batch loader cannot be lowered.
//
// Not returned in practice: the generator asks this dialect for its DEFAULT
// batch load and gets the window form, which MariaDB does have. It exists so
// that a caller reaching for the lateral form by name learns why it is absent
// rather than finding a silently different plan.
var ErrNoLateral = errors.New(
	"compile/mariadb: MariaDB has no LATERAL. The window form of the batch loader is used " +
		"instead, which MariaDB does support")

// TopNBatch is the default per-parent batch load.
//
// MySQL's default is the lateral form, chosen by measurement. MariaDB has no
// LATERAL, so the window form is the default here — not a fallback for a
// caller's odd data, but the only one available. Its cost tracks the total
// child count rather than the rows returned, and that is a real difference in
// what a MariaDB target pays for a relation load.
func TopNBatch(table string, cols []string, key, keyType string, live mysql.Live) string {
	return mysql.TopNWindow(table, cols, key, keyType, live)
}
