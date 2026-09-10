package mysql_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/mysql"
	"github.com/gsoultan/storm/compile/pgsql"
)

// The one MySQL difference that is not a spelling.
//
// compile/pgsql says RETURNING "is not an optimisation here, it is the only
// correct way to learn a generated id or a DEFAULT the database computed.
// Reading them back with a second SELECT races every other writer." MySQL 8 has
// no RETURNING, and neither substitute closes the gap: LAST_INSERT_ID() reports
// an AUTO_INCREMENT and nothing else — not a uuid default, not a server
// timestamp, not a generated column — and a following SELECT is the race.
//
// So the insert REFUSES rather than dropping the clause. Dropping it would
// compile, run, and hand back a zero id.
func TestInsertRefusesToPretendItCanReturn(t *testing.T) {
	_, err := mysql.InsertStmt("t", []string{"id", "email"}, []string{"id"})
	if !errors.Is(err, mysql.ErrNoReturning) {
		t.Fatalf("an insert wanting values back returned %v; it must refuse", err)
	}
	for _, want := range []string{"LAST_INSERT_ID", "racing", "MariaDB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	// With nothing to return it is an ordinary insert.
	got, err := mysql.InsertStmt("t", []string{"id", "email"}, nil)
	if err != nil {
		t.Fatalf("a plain insert was refused: %v", err)
	}
	if got != "INSERT INTO `t` (`id`, `email`) VALUES (?, ?)" {
		t.Fatalf("InsertStmt = %s", got)
	}
	// Every placeholder is identical, because position binds here.
	if strings.Contains(got, "?1") || strings.Contains(got, "$") {
		t.Errorf("an ordinal reached a bare-placeholder back end: %s", got)
	}
}

// A lock mode is an INDEX into the generated statement-cache array, and the
// numbering belongs to the generated Query rather than to a back end. If the
// two dialects ever disagree, a MySQL package would take a different lock from
// the one the caller asked for — silently, because both are valid SQL.
func TestLockModeNumberingMatchesPostgres(t *testing.T) {
	if mysql.NumLockModes != pgsql.NumLockModes {
		t.Fatalf("MySQL has %d lock modes, PostgreSQL %d — the cache arrays would differ in width",
			mysql.NumLockModes, pgsql.NumLockModes)
	}
	for m, want := range map[int]string{
		mysql.LockNone:             "",
		mysql.LockUpdate:           " FOR UPDATE",
		mysql.LockUpdateNoWait:     " FOR UPDATE NOWAIT",
		mysql.LockUpdateSkipLocked: " FOR UPDATE SKIP LOCKED",
		mysql.LockShare:            " FOR SHARE",
		mysql.LockShareNoWait:      " FOR SHARE NOWAIT",
		mysql.LockShareSkipLocked:  " FOR SHARE SKIP LOCKED",
	} {
		if got := mysql.LockSuffix(m); got != want {
			t.Errorf("LockSuffix(%d) = %q, want %q", m, got, want)
		}
	}
	// The names must line up slot for slot, not merely count the same.
	for m := 0; m < pgsql.NumLockModes; m++ {
		p := pgsql.LockSuffix(pgsql.LockMode(m))
		if got := mysql.LockSuffix(m); got != p {
			t.Errorf("slot %d: MySQL locks %q where PostgreSQL locks %q — a caller asking "+
				"for one would get the other", m, got, p)
		}
	}
}

// MySQL has no NULLS FIRST / NULLS LAST — Error 1064 on 8.4.11 — and sorts
// NULLs first ascending, last descending. Those are the two placements storm
// can ask for, so each maps to the plain form rather than to an ISNULL() key
// that would cost a sort to say what the server already does.
func TestOrderTermDoesNotEmitNullsPlacement(t *testing.T) {
	for dir := 0; dir < mysql.NDirections; dir++ {
		got := mysql.OrderTerm(dir, mysql.Ident("age"))
		if strings.Contains(strings.ToUpper(got), "NULLS") {
			t.Errorf("OrderTerm(%d) = %q; MySQL rejects a NULLS placement", dir, got)
		}
	}
	if mysql.NDirections != pgsql.NDirections {
		t.Errorf("the dialects distinguish different numbers of orderings (%d vs %d); "+
			"the direction is a token-stream index, not a back end's choice",
			mysql.NDirections, pgsql.NDirections)
	}
}

// Keyset pagination crosses unchanged: MySQL supports row comparison, measured.
func TestRowComparisonIsAvailable(t *testing.T) {
	got := mysql.TupleOpen + mysql.Ident("age") + mysql.TupleSep + mysql.Ident("id") + mysql.TupleClose +
		mysql.RowCmpOp(0) + mysql.TupleOpen + mysql.Placeholder + mysql.TupleSep + mysql.Placeholder + mysql.TupleClose
	if got != "(`age`, `id`) > (?, ?)" {
		t.Fatalf("row comparison = %s", got)
	}
}
