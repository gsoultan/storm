package msbench

// Which of M10's constructs the engine actually has.
//
// The milestone's exit gate names OUTPUT, MERGE, TVP bulk and a paging gate,
// and every one of them was a guess from documentation until this ran. It runs
// against Azure SQL Edge because that is the only SQL Server engine with an
// arm64 image — full SQL Server is amd64 only, so a developer on Apple silicon
// has no local server without it. CI runs the real one.

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/microsoft/go-mssqldb"
)

func try(t *testing.T, db *sql.DB, name, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		t.Errorf("%s: NOT available — %v", name, firstLine(err.Error()))
		return
	}
	t.Logf("%-24s yes", name)
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	if len(s) > 110 {
		return s[:110]
	}
	return s
}

func TestEveryConstructM10NeedsExists(t *testing.T) {
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skip(dsnEnv + " unset")
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, _ = db.Exec(`DROP TYPE IF EXISTS storm_ids`)
	_, _ = db.Exec(`DROP TABLE IF EXISTS t`)
	try(t, db, "create table", `CREATE TABLE t (id UNIQUEIDENTIFIER PRIMARY KEY, n INT NOT NULL, s NVARCHAR(80) NOT NULL)`)
	try(t, db, "INSERT ... OUTPUT", `INSERT INTO t (id, n, s) OUTPUT INSERTED.id, INSERTED.n VALUES (NEWID(), 1, 'a')`)
	try(t, db, "UPDATE ... OUTPUT", `UPDATE t SET n = 2 OUTPUT INSERTED.n WHERE n = 1`)
	try(t, db, "DELETE ... OUTPUT", `DELETE FROM t OUTPUT DELETED.id WHERE n = 99`)
	try(t, db, "OFFSET/FETCH paging", `SELECT id FROM t ORDER BY id OFFSET 0 ROWS FETCH NEXT 10 ROWS ONLY`)
	try(t, db, "MERGE", `MERGE t AS tgt USING (SELECT NEWID() AS id, 5 AS n, 'm' AS s) AS src
	  ON tgt.id = src.id
	  WHEN MATCHED THEN UPDATE SET n = src.n
	  WHEN NOT MATCHED THEN INSERT (id, n, s) VALUES (src.id, src.n, src.s);`)
	try(t, db, "user-defined table type", `CREATE TYPE storm_ids AS TABLE (v UNIQUEIDENTIFIER)`)
	try(t, db, "CTE", `WITH c AS (SELECT id FROM t) SELECT * FROM c`)
	try(t, db, "WITH RECURSIVE (no kw)", `WITH c AS (SELECT 1 AS d UNION ALL SELECT d+1 FROM c WHERE d < 3) SELECT * FROM c OPTION (MAXRECURSION 10)`)
	try(t, db, "window function", `SELECT row_number() OVER (PARTITION BY n ORDER BY id) FROM t`)
	try(t, db, "CROSS APPLY (lateral)", `SELECT t.id FROM t CROSS APPLY (SELECT TOP 1 id FROM t x WHERE x.n = t.n) y`)
	try(t, db, "JSON value", `SELECT JSON_VALUE('{"a":1}', '$.a')`)
	try(t, db, "OPENJSON (list param)", `SELECT value FROM OPENJSON('[1,2,3]')`)
	try(t, db, "partial/filtered index", `CREATE INDEX ix_t_n ON t (n) WHERE n > 0`)
	try(t, db, "FOR UPDATE equivalent", `SELECT id FROM t WITH (UPDLOCK, ROWLOCK) WHERE n = 1`)
}
