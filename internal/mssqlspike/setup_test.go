package msbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/gsoultan/storm/compile/msddl"
)

// The gate's fixture: the schema applied, and two rows to execute against.
//
// Values, not an empty schema. A statement that PREPAREs against empty tables
// proves the text parses; only a row proves the CONVERSIONS work — a uuid
// arriving as a string through OPENJSON, a DATETIMEOFFSET coming back, a
// computed column that has to evaluate.
var (
	orgID    = uuid.NewString()
	memberID = uuid.NewString()
)

func newUUID() string { return uuid.NewString() }

// jsonKeys is a bound key list, the way a batch loader passes one: a JSON array
// of the parents' keys, unpacked server-side by OPENJSON.
func jsonKeys() string {
	b, _ := json.Marshal([]string{orgID})
	return string(b)
}

func setup(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	s := msSchema(t)
	ddl, err := msddl.Create(s)
	if err != nil {
		t.Fatalf("msddl refused a portable model: %v", err)
	}
	drop(t, db, "ms_members", "ms_orgs")
	for _, stmt := range split(ddl) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("SQL Server refused\n  %s\n%v", stmt, err)
		}
	}
	t.Cleanup(func() { drop(t, db, "ms_members", "ms_orgs") })

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed failed\n  %s\n%v", q, err)
		}
	}
	exec(`INSERT INTO [ms_orgs] ([id],[name],[seats],[ratio],[balance],[active],[opened],[note],[doc]) `+
		`VALUES (@p1, 'Acme', 10, 1.5, 12.3400, 1, '2026-01-01', 'n', '{}')`, orgID)
	exec(`INSERT INTO [ms_members] ([id],[email],[rank],[org_id]) VALUES (@p1, 'a@b.c', 1, @p2)`,
		memberID, orgID)
}
