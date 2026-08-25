package planspike_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gsoultan/raorm"
	"github.com/gsoultan/raorm/runtime"
)

// The M5 gate query: a window function over a CTE with a lateral join — the
// analytical shape the native IR cannot express yet — fully typed, zero `any`
// in the scan path, against live PostgreSQL.
//
// The scanner here is HAND-written and hand-registered: the P2 split again.
// "Is the SQLQuery runtime design right?" and "can the generator emit the
// scanner?" must fail separately; the generation half is proven in the CLI
// tests, where a database is available at generate time.
type earnerRow struct {
	Email    string
	OrgName  string
	Rank     int64
	OrgUsers int64
}

var topPerOrg = raorm.SQL[earnerRow](`
	WITH ranked AS (
		SELECT u.email, u.org_id,
		       row_number() OVER (PARTITION BY u.org_id ORDER BY u.email) AS rn
		FROM users u
	)
	SELECT r.email, o.name AS org_name, r.rn AS rank, l.org_users
	FROM ranked r
	JOIN orgs o ON o.id = r.org_id
	JOIN LATERAL (
		SELECT count(*) AS org_users FROM users u2 WHERE u2.org_id = r.org_id
	) l ON true
	WHERE r.rn <= $1
	ORDER BY o.name, r.rn
	LIMIT $2`)

func init() {
	raorm.RegisterScanner(func(rv [][]byte, r *earnerRow, sl *runtime.Slab) error {
		r.Email = sl.Str(rv[0])
		r.OrgName = sl.Str(rv[1])
		r.Rank = runtime.Int8(rv[2])
		r.OrgUsers = runtime.Int8(rv[3])
		return nil
	})
}

func TestSQLQuery_AnalyticalShapeFullyTyped(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)

	count.Reset()
	rows, err := topPerOrg.Query(ctx, ex, int64(2), int64(20))
	if err != nil {
		t.Fatal(err)
	}
	if n := count.RoundTrips(); n != 1 {
		t.Errorf("%d round trips, want 1 — however analytical, it is one statement", n)
	}
	if len(rows) != 20 {
		t.Fatalf("got %d rows, want 20", len(rows))
	}
	for i, r := range rows {
		if r.Rank < 1 || r.Rank > 2 {
			t.Errorf("row %d: rank %d escaped the window predicate", i, r.Rank)
		}
		if r.OrgUsers != usersPerOrg {
			t.Errorf("row %d: lateral count = %d, want %d", i, r.OrgUsers, usersPerOrg)
		}
		if r.Email == "" || r.OrgName == "" {
			t.Errorf("row %d: text columns did not decode: %+v", i, r)
		}
	}
	// Two ranks per org, ordered: rows pair up under one org name.
	if rows[0].OrgName != rows[1].OrgName || rows[0].Rank != 1 || rows[1].Rank != 2 {
		t.Errorf("ordering is off: %+v then %+v", rows[0], rows[1])
	}
}

// The argument count is checked before anything reaches the server.
func TestSQLQuery_ArgumentCountIsChecked(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	count.Reset()

	_, err := topPerOrg.Query(ctx, ex, int64(2))
	if err == nil {
		t.Fatal("one argument for a two-placeholder statement must fail")
	}
	if !strings.Contains(err.Error(), "wants 2") || !strings.Contains(err.Error(), "got 1") {
		t.Errorf("the error should carry both numbers, got: %v", err)
	}
	if count.RoundTrips() != 0 {
		t.Error("a mis-called query still reached the server")
	}
}

// A query nothing generated for says how to fix itself.
func TestSQLQuery_MissingScannerNamesTheFix(t *testing.T) {
	type orphanRow struct{ X int64 }
	q := raorm.SQL[orphanRow](`SELECT 1 AS x`)
	_, err := q.Query(context.Background(), nil)
	if err == nil {
		t.Fatal("a query with no registered scanner must fail")
	}
	if !strings.Contains(err.Error(), "raorm generate") {
		t.Errorf("the error should name the fix, got: %v", err)
	}
}
