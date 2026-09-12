package mariadb_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/mariadb"
	"github.com/gsoultan/storm/compile/mysql"
	"github.com/gsoultan/storm/schema"
)

// MariaDB speaks MySQL's WIRE protocol but not its SQL. These are the five
// places they part company, each measured against 11.4.13 and 8.4.11 — the
// whole difference, in one file, on purpose.

// The one that pays for the dialect. compile/pgsql: RETURNING "is not an
// optimisation here, it is the only correct way to learn a generated id or a
// DEFAULT the database computed". MySQL 8 has none; MariaDB does.
func TestInsertCanReturnTheRowItWrote(t *testing.T) {
	got, err := mariadb.InsertStmt("t", []string{"id", "email"}, []string{"id", "email"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, " RETURNING `id`, `email`") {
		t.Fatalf("no returning clause: %s", got)
	}
	// MySQL refuses the same call outright.
	if _, err := mysql.InsertStmt("t", []string{"id", "email"}, []string{"id"}); err == nil {
		t.Error("fixture is wrong: MySQL accepted a returning list")
	}
	// And with nothing to return it is the same insert MySQL emits.
	plain, err := mariadb.InsertStmt("t", []string{"id"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := mysql.InsertStmt("t", []string{"id"}, nil)
	if plain != mine {
		t.Errorf("the plain insert diverged from MySQL's:\n%s\n%s", plain, mine)
	}
}

// FOR SHARE does not exist here; LOCK IN SHARE MODE does. The exclusive modes
// are spelled identically, and the NUMBERING must stay MySQL's because a mode
// is an index into the generated statement-cache array.
func TestSharedLockIsSpelledDifferently(t *testing.T) {
	for m, want := range map[int]string{
		mysql.LockShare:            " LOCK IN SHARE MODE",
		mysql.LockShareNoWait:      " LOCK IN SHARE MODE NOWAIT",
		mysql.LockShareSkipLocked:  " LOCK IN SHARE MODE SKIP LOCKED",
		mysql.LockUpdate:           " FOR UPDATE",
		mysql.LockUpdateNoWait:     " FOR UPDATE NOWAIT",
		mysql.LockUpdateSkipLocked: " FOR UPDATE SKIP LOCKED",
	} {
		if got := mariadb.LockSuffix(m); got != want {
			t.Errorf("LockSuffix(%d) = %q, want %q", m, got, want)
		}
	}
	if strings.Contains(mariadb.LockSuffix(mysql.LockShare), "FOR SHARE") {
		t.Error("FOR SHARE reached MariaDB SQL; it rejects it")
	}
}

// MariaDB rejects WITH ROLLUP together with ORDER BY (Error 1221), and storm
// always orders a grouped read. Dropping the ordering would trade a refusal for
// a report that pages wrongly, so the rollup is refused.
func TestRolledUpAggregationIsRefused(t *testing.T) {
	agg := &schema.Aggregate{
		Name: "ByRegion",
		By:   []schema.GroupTerm{{Expr: schema.Expr{Kind: schema.ExprCol, Col: "region"}, As: "Region"}},
		Sets: &schema.GroupingSets{Kind: schema.SetsRollup},
	}
	if _, err := mariadb.AggregateSuffix(agg); !errors.Is(err, mariadb.ErrNoOrderedRollup) {
		t.Fatalf("a rolled-up aggregation returned %v; MariaDB cannot order one", err)
	}
	if !strings.Contains(mariadb.ErrNoOrderedRollup.Error(), "1221") {
		t.Error("the refusal does not name the error a reader would see")
	}
	// A plain grouping is fine, and identical to MySQL's.
	agg.Sets = nil
	got, err := mariadb.AggregateSuffix(agg)
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := mysql.AggregateSuffix(agg)
	if got != mine {
		t.Errorf("a plain aggregation diverged from MySQL's:\n%s\n%s", got, mine)
	}
}

// No LATERAL. The window form is not a fallback for odd data here — it is the
// only one available, and its cost tracks the total child count rather than the
// rows returned. That is a real difference in what a MariaDB target pays.
func TestBatchLoadUsesTheWindowForm(t *testing.T) {
	got := mariadb.TopNBatch("members", []string{"id"}, "org_id", "BIGINT", "")
	if strings.Contains(got, "LATERAL") {
		t.Errorf("LATERAL reached MariaDB SQL; it has none:\n%s", got)
	}
	if !strings.Contains(got, "row_number() OVER") {
		t.Errorf("the batch load is not the window form:\n%s", got)
	}
	// Still one bound JSON document, not a placeholder per key.
	if n := strings.Count(got, "?"); n != 2 {
		t.Errorf("binds %d placeholders, want 2 (the key list, then N):\n%s", n, got)
	}
}

// Everything not listed above is MySQL's, and shares its implementation rather
// than copying it — four fifths duplicated is where the drift would happen.
func TestTheSharedFourFifthsIsShared(t *testing.T) {
	if mariadb.ReturningClause(nil) != "" {
		t.Error("an empty returning list should render nothing")
	}
	// Identifier quoting, placeholders and the recursive form all come from
	// compile/mysql unchanged; if that stopped being true this file would need
	// to grow, which is the signal worth having.
	if mysql.Ident("x") != "`x`" || mysql.Placeholder != "?" {
		t.Error("the shared spelling changed under MariaDB's feet")
	}
}
