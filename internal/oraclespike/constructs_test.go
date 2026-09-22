package orabench

// Result 1: does the engine have what M11's gate names?
//
// The same question M10's spike asked of SQL Server, and asked the same way —
// by RUNNING each construct rather than reading the documentation. M10's list
// was fifteen guesses from documentation, and running them moved two.

import (
	"database/sql"
	"strings"
	"testing"
)

// probe runs one statement and REPORTS whether the server accepted it.
//
// It does not fail. A spike reports; a gate refuses, and there is no Oracle
// back end to protect yet — a construct that turns out to be missing is this
// file's OUTPUT, not its error. The three facts M12's kill criterion actually
// rests on are asserted in empty_test.go, where failing is the point.
func probe(t *testing.T, db *sql.DB, name, stmt string) bool {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		t.Logf("  %-46s NO   %v", name, firstLine(err))
		return false
	}
	t.Logf("  %-46s yes", name)
	return true
}

func query1(t *testing.T, db *sql.DB, name, stmt string) bool {
	t.Helper()
	rows, err := db.Query(stmt)
	if err != nil {
		t.Logf("  %-46s NO   %v", name, firstLine(err))
		return false
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Logf("  %-46s NO   %v", name, firstLine(err))
			return false
		}
	}
	t.Logf("  %-46s yes", name)
	return true
}

// firstLine trims an ORA- message to its number and sentence. go-ora appends
// "error occur at position: n" on a second line, which is about the driver's
// buffer rather than the statement.
func firstLine(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func TestEveryConstructM11NeedsExists(t *testing.T) {
	db := open(t)
	drop(db, "TABLE", "c_probe PURGE")
	mustExec(t, db, `CREATE TABLE c_probe (a NUMBER PRIMARY KEY, b VARCHAR2(40), d DATE)`)
	t.Cleanup(func() { drop(db, "TABLE", "c_probe PURGE") })
	mustExec(t, db, `INSERT INTO c_probe (a, b) VALUES (1, 'one')`)
	mustExec(t, db, `INSERT INTO c_probe (a, b) VALUES (2, 'two')`)

	// The paging gate. Oracle got OFFSET/FETCH in 12c; before that it was a
	// ROWNUM subquery, which is a different shape and would be a lowering of
	// its own.
	query1(t, db, "OFFSET FETCH",
		`SELECT a FROM c_probe ORDER BY a OFFSET 1 ROWS FETCH NEXT 1 ROWS ONLY`)

	// The upsert. Oracle has MERGE and no ON CONFLICT, so the whole statement
	// comes from the back end — the same cut compile/mssql made.
	probe(t, db, "MERGE", `MERGE INTO c_probe t USING (SELECT 3 a, 'three' b FROM dual) s
		ON (t.a = s.a)
		WHEN MATCHED THEN UPDATE SET t.b = s.b
		WHEN NOT MATCHED THEN INSERT (a, b) VALUES (s.a, s.b)`)

	query1(t, db, "recursive CTE", `WITH r(n) AS (
		SELECT 1 FROM dual UNION ALL SELECT n+1 FROM r WHERE n < 3) SELECT n FROM r`)

	query1(t, db, "window functions",
		`SELECT row_number() OVER (ORDER BY a) FROM c_probe`)

	// LATERAL, which is CROSS APPLY here as it is on SQL Server. 12c.
	query1(t, db, "CROSS APPLY",
		`SELECT p.a, x.y FROM c_probe p CROSS APPLY (SELECT p.a * 2 AS y FROM dual) x`)

	// One bound value for a whole list — the trick that keeps an IN list from
	// minting a statement per arity. SQL Server's is OPENJSON, MySQL's is
	// JSON_TABLE, and Oracle has JSON_TABLE too.
	query1(t, db, "JSON_TABLE for an IN list",
		`SELECT v FROM JSON_TABLE('[1,2,3]', '$[*]' COLUMNS (v NUMBER PATH '$'))`)

	query1(t, db, "GROUPING SETS",
		`SELECT b, count(*) FROM c_probe GROUP BY GROUPING SETS ((b), ())`)
	query1(t, db, "GROUPING()",
		`SELECT GROUPING(b) FROM c_probe GROUP BY CUBE(b)`)

	// Row locks. Three probes, not one, because the first run of this spike
	// combined them and could not say which half Oracle refused.
	query1(t, db, "FOR UPDATE SKIP LOCKED",
		`SELECT a FROM c_probe FOR UPDATE SKIP LOCKED`)

	// THE WORK-QUEUE SHAPE. `LIMIT n FOR UPDATE SKIP LOCKED` is one statement
	// on PostgreSQL, MySQL and SQL Server, and it is how every job table storm
	// generates for is read. Oracle implements FETCH FIRST as an inline view
	// with a window function, and ORA-02014 refuses FOR UPDATE against one.
	query1(t, db, "FETCH FIRST with FOR UPDATE",
		`SELECT a FROM c_probe ORDER BY a FETCH FIRST 1 ROWS ONLY FOR UPDATE SKIP LOCKED`)

	// And the idiom that replaces it, so the README can say what the lowering
	// has to be rather than only what it cannot be.
	query1(t, db, "ROWNUM with FOR UPDATE",
		`SELECT a FROM c_probe WHERE ROWNUM <= 1 FOR UPDATE SKIP LOCKED`)

	// The soft-delete question. Oracle has NO partial index — but a
	// function-based unique index on a CASE is the same guarantee, because a
	// NULL key is not indexed. If this works, the live-scoped unique that
	// MySQL cannot take ports here, exactly as it did to SQL Server's
	// filtered index.
	probe(t, db, "partial UNIQUE via a function-based index",
		`CREATE UNIQUE INDEX c_probe_live ON c_probe (CASE WHEN d IS NULL THEN b END)`)

	// Native BOOLEAN is 23c. Before it, NUMBER(1) plus a CHECK — which storm
	// would have to generate from one s.Bool() declaration either way, so this
	// decides whether the model's bool is a type or a convention.
	drop(db, "TABLE", "c_bool PURGE")
	if probe(t, db, "native BOOLEAN (23c)", `CREATE TABLE c_bool (f BOOLEAN)`) {
		t.Cleanup(func() { drop(db, "TABLE", "c_bool PURGE") })
	}

	// Server-generated keys without a sequence object to manage. 12c.
	drop(db, "TABLE", "c_ident PURGE")
	if probe(t, db, "IDENTITY column",
		`CREATE TABLE c_ident (id NUMBER GENERATED ALWAYS AS IDENTITY PRIMARY KEY, x NUMBER)`) {
		t.Cleanup(func() { drop(db, "TABLE", "c_ident PURGE") })
	}

	// 128-byte identifiers are 12.2 and later; before that 30, which is
	// shorter than several names storm generates (ck_<table>_<column> on a
	// long table). Worth knowing which world M11 is in.
	long := "c_" + strings.Repeat("x", 126)
	drop(db, "TABLE", long+" PURGE")
	if probe(t, db, "128-character identifiers", `CREATE TABLE `+long+` (a NUMBER)`) {
		t.Cleanup(func() { drop(db, "TABLE", long+" PURGE") })
	}
}

// The identifier case question, which has no equivalent on any target storm
// already has.
//
// PostgreSQL folds an unquoted name to LOWER; Oracle folds it to UPPER. storm
// quotes every identifier it writes, so generation is unaffected — but `storm
// import` reads a CATALOGUE, and what it finds there is USERS, not users. A
// model generated from that would declare Go fields from shouting names, and a
// model written by hand would then diff against it forever.
func TestUnquotedIdentifiersFoldUp(t *testing.T) {
	db := open(t)
	drop(db, "TABLE", "fold_probe PURGE")
	mustExec(t, db, `CREATE TABLE fold_probe (a NUMBER)`)
	t.Cleanup(func() { drop(db, "TABLE", "fold_probe PURGE") })

	var name string
	err := db.QueryRow(
		`SELECT table_name FROM user_tables WHERE upper(table_name) = 'FOLD_PROBE'`).Scan(&name)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("declared `fold_probe`, catalogue says %q", name)
	if name != "FOLD_PROBE" {
		t.Errorf("expected the catalogue to shout; got %q — the import path may be simpler than feared", name)
	}

	// And a QUOTED lowercase name survives, which is what storm's own DDL
	// would produce. Both can exist at once, which is the trap: `users` and
	// `USERS` are two tables.
	drop(db, "TABLE", `"fold_probe" PURGE`)
	mustExec(t, db, `CREATE TABLE "fold_probe" (a NUMBER)`)
	t.Cleanup(func() { drop(db, "TABLE", `"fold_probe" PURGE`) })

	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM user_tables WHERE upper(table_name) = 'FOLD_PROBE'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	t.Logf("after declaring both `fold_probe` and \"fold_probe\": %d table(s)", n)
	if n != 2 {
		t.Errorf("expected 2 tables, got %d", n)
	}
}
