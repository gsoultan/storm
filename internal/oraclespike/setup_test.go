package orabench

// M11's two questions, asked before M11 starts — the rule docs/PLAN.md set at
// M9 and applied again at M10: the driver decision is "the estimate to make
// before starting, not after."
//
// A SEPARATE module, so storm's own go.mod gains no Oracle dependency for a
// measurement.
//
// Unlike M10's spike, this one has a THIRD question, and it is the one that
// matters most: docs/PLAN.md makes the capability model's ability to carry
// Oracle the KILL CRITERION for M12. "capability model cannot carry Oracle →
// Mongo is cancelled". So the semantics are probed here, executed, before
// anything is built on the assumption that they can be.

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/sijms/go-ora/v2"
)

const dsnEnv = "STORM_ORACLE_DSN"

// open connects, or skips. Oracle Free 23.5 and later ship a native arm64
// image, so unlike SQL Server this is the SAME engine on a developer's laptop
// and in CI — there is no Azure SQL Edge equivalent to caveat.
//
//	container run -d --name storm-oracle -p 1521:1521 \
//	  -e ORACLE_PASSWORD=Storm1Passw0rd gvenzl/oracle-free:slim
//	STORM_ORACLE_DSN='oracle://system:Storm1Passw0rd@localhost:1521/FREEPDB1' go test ./...
func open(t testing.TB) *sql.DB {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skip(dsnEnv + " unset")
	}
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Skipf("no Oracle reachable at %s: %v", dsnEnv, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// drop removes an object without caring whether it was there. Oracle has no
// DROP ... IF EXISTS, so the alternative is a PL/SQL block per object.
func drop(db *sql.DB, kind, name string) {
	_, _ = db.Exec("DROP " + kind + " " + name)
}

// mustExec fails the test, naming the statement. Oracle's errors are ORA-nnnnn
// and the number is the useful part, so the whole text is printed.
func mustExec(t testing.TB, db *sql.DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(sql, args...); err != nil {
		t.Fatalf("%s\n  %v", sql, err)
	}
}
