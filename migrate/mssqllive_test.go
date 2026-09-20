package migrate_test

// The SQL Server migration path, APPLIED.
//
// docs/PRODUCTION-READINESS.md P6.7: a test that does not execute against the
// target proves the generator is consistent with itself and nothing more. This
// file exists because the SQL Server half of migrate's DDL seam is five
// statements that are spelled differently, and four of the five fail in a way
// a text assertion cannot see —
//
//   - ADD COLUMN parses as a column literally named COLUMN,
//   - ALTER COLUMN without NULL/NOT NULL silently takes ANSI_NULL_DFLT_ON's
//     answer,
//   - ADD CONSTRAINT ... DEFAULT on a column that has one is error 1781,
//   - DROP INDEX without ON is a syntax error.
//
// The shape is a ROUND TRIP, twice: build the model, apply the plan, and
// demand that the next plan be EMPTY. An empty second plan is the only
// evidence that normalisation and introspection agree — every width, every
// default the server parenthesised, every enum that became a CHECK. A
// field-by-field assertion would pass while the two disagreed about all of it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/runtime/msdrv"
	"github.com/gsoultan/storm/schema"
	msintro "github.com/gsoultan/storm/schema/mssql"
)

type migStatus string

func (migStatus) EnumValues() []string { return []string{"new", "paid"} }

type migOrg struct {
	storm.Model
	Name    string
	Seats   int32
	Balance storm.Decimal
	Active  bool
	Opened  time.Time
	Note    *string
	Status  migStatus
}

func (o *migOrg) Schema(t *storm.Table) {
	t.Col(&o.Name).Size(200)
	t.Col(&o.Balance).Numeric(19, 4)
	t.Col(&o.Opened).Date()
	t.Col(&o.Seats).Default("1")
	t.Unique(&o.Name)
	t.Index(&o.Active)
}

type migMember struct {
	storm.Model
	Email string
	Rank  int64
	Org   migOrg
}

func msDialer(t *testing.T, database string) migrate.MSSQLDialer {
	t.Helper()
	addr := os.Getenv("STORM_MSSQL_ADDR")
	if addr == "" {
		t.Skip("STORM_MSSQL_ADDR unset")
	}
	return func(ctx context.Context, db string) (migrate.MSSQLConn, func(), error) {
		if db == "" {
			db = database
		}
		c, err := msdrv.Open(ctx, msdrv.Config{
			Addr: addr, User: "sa", Password: os.Getenv("STORM_MSSQL_PASSWORD"),
			Database: db, TLS: msdrv.TLSDisabled,
		})
		if err != nil {
			return nil, func() {}, err
		}
		return c, func() { c.Close() }, nil
	}
}

// targetDB gives the test its own database, because the thing under test
// CREATES one of its own alongside it and a shared target would let two runs
// migrate each other's tables.
func targetDB(t *testing.T) (context.Context, migrate.MSSQLDialer, string) {
	t.Helper()
	name := fmt.Sprintf("storm_migrate_%d", os.Getpid())
	ctx := context.Background()
	dial := msDialer(t, name)

	admin, closeAdmin, err := dial(ctx, "master")
	if err != nil {
		t.Skipf("no SQL Server reachable: %v", err)
	}
	drop := func() {
		_, _ = admin.Exec(context.Background(), "IF DB_ID(N'"+name+"') IS NOT NULL BEGIN "+
			"ALTER DATABASE ["+name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; "+
			"DROP DATABASE ["+name+"]; END", nil)
	}
	drop()
	if _, err := admin.Exec(ctx, "CREATE DATABASE ["+name+"]", nil); err != nil {
		closeAdmin()
		t.Fatalf("create target database: %v", err)
	}
	t.Cleanup(func() { drop(); closeAdmin() })
	return ctx, dial, name
}

// apply runs a plan the way a migration runner would: statement by statement,
// in order, stopping at the first refusal. No transaction — a SQL Server
// migration file is applied as a batch, and wrapping it here would hide a
// statement that cannot run in one.
func apply(t *testing.T, ctx context.Context, dial migrate.MSSQLDialer, p migrate.Plan) {
	t.Helper()
	c, closeC, err := dial(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeC()
	for i, ch := range p.Changes {
		if strings.HasPrefix(strings.TrimSpace(ch.SQL), "--") {
			continue
		}
		if _, err := c.Exec(ctx, ch.SQL, nil); err != nil {
			t.Fatalf("change %d refused by the server:\n%s\n%v", i, ch.SQL, err)
		}
	}
}

func planFor(t *testing.T, ctx context.Context, dial migrate.MSSQLDialer, s *schema.Schema) migrate.Plan {
	t.Helper()
	p, err := migrate.ForMSSQL(ctx, dial, "dbo", s)
	if err != nil {
		t.Fatalf("ForMSSQL: %v", err)
	}
	return p
}

func build(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := storm.Build(&migOrg{}, &migMember{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMSSQLMigrationRoundTrip(t *testing.T) {
	ctx, dial, _ := targetDB(t)
	want := build(t)

	// 1. Empty database to the model.
	first := planFor(t, ctx, dial, want)
	if first.Empty() {
		t.Fatal("an empty database produced no plan")
	}
	apply(t, ctx, dial, first)

	// 2. The property everything else rests on. A non-empty plan here means
	//    the model normalised to something the introspector reads back
	//    differently — and every one of those is a migration that reapplies
	//    itself forever.
	if again := planFor(t, ctx, dial, want); !again.Empty() {
		t.Fatalf("re-diff after applying the plan is not empty — normalisation and "+
			"introspection disagree:\n%s", again.SQL())
	}
}

// Every renderer whose SQL Server spelling differs, applied. Each step is its
// own plan so a failure names the statement that the server refused.
func TestMSSQLAltersApply(t *testing.T) {
	ctx, dial, _ := targetDB(t)
	apply(t, ctx, dial, planFor(t, ctx, dial, build(t)))

	steps := []struct {
		name  string
		edit  func(*schema.Table)
		wants string // a fragment the plan must contain, so a no-op step fails
	}{
		{"add a column", func(tb *schema.Table) {
			tb.Columns = append(tb.Columns, &schema.Column{
				Name: "region", Type: schema.Type{Name: schema.TypeVarchar, Size: 40},
			})
		}, "ADD [region]"},

		{"widen a column", func(tb *schema.Table) {
			tb.Column("name").Type.Size = 300
		}, "ALTER COLUMN [name]"},

		{"make a column NOT NULL", func(tb *schema.Table) {
			tb.Column("note").NotNull = true
		}, "NOT NULL;"},

		{"make it nullable again", func(tb *schema.Table) {
			tb.Column("note").NotNull = false
		}, "ALTER COLUMN [note]"},

		{"replace a default", func(tb *schema.Table) {
			tb.Column("seats").Default = "2"
		}, "DF_mig_orgs_seats"},

		{"drop a default", func(tb *schema.Table) {
			tb.Column("seats").Default = ""
		}, "sys.default_constraints"},

		{"drop an index", func(tb *schema.Table) {
			tb.Indexes = nil
		}, "DROP INDEX"},

		{"drop a unique constraint", func(tb *schema.Table) {
			tb.Uniques = nil
		}, "DROP CONSTRAINT"},

		{"drop a column", func(tb *schema.Table) {
			cols := tb.Columns[:0]
			for _, c := range tb.Columns {
				if c.Name != "region" {
					cols = append(cols, c)
				}
			}
			tb.Columns = cols
		}, "DROP COLUMN [region]"},
	}

	// The edits accumulate: each step's model is every edit so far, which is
	// what a sequence of migrations actually looks like.
	var edits []func(*schema.Table)
	for _, st := range steps {
		edits = append(edits, st.edit)
		t.Run(st.name, func(t *testing.T) {
			want := build(t)
			tb := want.Table("mig_orgs")
			if tb == nil {
				t.Fatal("mig_orgs is not in the model")
			}
			for _, e := range edits {
				e(tb)
			}

			p := planFor(t, ctx, dial, want)
			if !strings.Contains(p.SQL(), st.wants) {
				t.Fatalf("plan does not contain %q, so this step tested nothing:\n%s", st.wants, p.SQL())
			}
			apply(t, ctx, dial, p)

			if again := planFor(t, ctx, dial, want); !again.Empty() {
				t.Fatalf("re-diff after applying is not empty:\n%s", again.SQL())
			}
		})
	}
}

// Automigrate, APPLIED.
//
// migrate.AutoMSSQL is the one path in storm that writes DDL to a database
// nobody reviewed first, so every property it claims is checked here against a
// server rather than against a rendering. The four that matter: it converges,
// it refuses to lose data, a failure leaves the schema where it began, and
// twenty replicas starting together apply it once.

func autoOpts(t *testing.T) migrate.AutoOptions {
	return migrate.AutoOptions{Logf: t.Logf}
}

func TestAutoMSSQLAppliesAndThenHasNothingToDo(t *testing.T) {
	ctx, dial, _ := targetDB(t)
	want := build(t)

	res, err := migrate.AutoMSSQL(ctx, dial, want, autoOpts(t))
	if err != nil {
		t.Fatalf("AutoMSSQL: %v", err)
	}
	if res.Empty() {
		t.Fatal("an empty database was migrated with no steps")
	}

	// Convergence, which is the whole promise. A second call that applied
	// anything would be a migration that runs on every start.
	again, err := migrate.AutoMSSQL(ctx, dial, want, autoOpts(t))
	if err != nil {
		t.Fatalf("second AutoMSSQL: %v", err)
	}
	if !again.Empty() {
		t.Fatalf("the second run applied %d step(s):\n%s", len(again.Applied), again.SQL())
	}
}

func TestAutoMSSQLRefusesToLoseDataAndAppliesNothing(t *testing.T) {
	ctx, dial, _ := targetDB(t)
	want := build(t)
	if _, err := migrate.AutoMSSQL(ctx, dial, want, autoOpts(t)); err != nil {
		t.Fatal(err)
	}

	// A model with a column removed. The refusal is of the WHOLE plan, not of
	// the destructive step in it: a half-applied schema is worse than an
	// unapplied one.
	shrunk := build(t)
	tb := shrunk.Table("mig_orgs")
	cols := tb.Columns[:0]
	for _, c := range tb.Columns {
		if c.Name != "note" {
			cols = append(cols, c)
		}
	}
	tb.Columns = cols

	_, err := migrate.AutoMSSQL(ctx, dial, shrunk, autoOpts(t))
	var de *migrate.DestructiveError
	if !errors.As(err, &de) {
		t.Fatalf("dropping a column returned %v, want *DestructiveError", err)
	}
	if !msColumnExists(t, ctx, dial, "mig_orgs", "note") {
		t.Error("the column was dropped by a call that returned DestructiveError")
	}
}

// One transaction, all or nothing — the property SQL Server's transactional
// DDL makes available and MySQL's does not.
//
// The failing step is real rather than injected: adding a NOT NULL column with
// no default to a table that HAS ROWS is exactly what the destructive flag
// warns about, and with AllowDestructive the server is allowed to refuse it.
// The step before it must not survive.
func TestAutoMSSQLRollsBackTheWholePlan(t *testing.T) {
	ctx, dial, _ := targetDB(t)
	if _, err := migrate.AutoMSSQL(ctx, dial, build(t), autoOpts(t)); err != nil {
		t.Fatal(err)
	}
	// Only the columns with neither a default nor NULL allowed: everything else
	// in this model has one, and naming fewer columns is fewer ways to be wrong
	// about a model that is not what this test is about.
	msExec(t, ctx, dial, "INSERT INTO [mig_orgs] ([name], [balance], [active], [opened], [status]) "+
		"VALUES ('acme', 0, 0, '2026-01-01', 'new')")

	// Appended in this order and applied in it: schema.Normalize sorts tables
	// and constraints but never columns, so declaration order is the order the
	// diff walks them and the order the plan applies them.
	want := build(t)
	tb := want.Table("mig_orgs")
	tb.Columns = append(tb.Columns,
		&schema.Column{Name: "first_ok", Type: schema.Type{Name: schema.TypeVarchar, Size: 10}},
		&schema.Column{Name: "then_fails", Type: schema.Type{Name: schema.TypeVarchar, Size: 10}, NotNull: true},
	)

	_, err := migrate.AutoMSSQL(ctx, dial, want, migrate.AutoOptions{
		AllowDestructive: true, Logf: t.Logf,
	})
	if err == nil {
		t.Fatal("a NOT NULL column with no default was added to a table with rows")
	}
	if msColumnExists(t, ctx, dial, "mig_orgs", "first_ok") {
		t.Errorf("the step before the failure stayed applied — the transaction did not roll back: %v", err)
	}
}

// Twenty replicas starting together apply the migration once. Four here,
// because four is enough to lose a race and twenty is only slower.
func TestAutoMSSQLConcurrentCallersApplyOnce(t *testing.T) {
	ctx, dial, _ := targetDB(t)
	want := build(t)

	const n = 4
	var wg sync.WaitGroup
	results := make([]migrate.Result, n)
	errs := make([]error, n)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = migrate.AutoMSSQL(ctx, dial, want, migrate.AutoOptions{
				LockWait: 60 * time.Second,
			})
		}(i)
	}
	wg.Wait()

	applied := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if !results[i].Empty() {
			applied++
		}
	}
	if applied != 1 {
		t.Errorf("%d of %d callers applied a plan; exactly one should have", applied, n)
	}
}

// The guard for the one thing SQL Server cannot do: point unqualified DDL at a
// chosen schema. Refusing beats writing into dbo while diffing something else.
func TestAutoMSSQLRefusesASchemaTheLoginDoesNotDefaultTo(t *testing.T) {
	ctx, dial, _ := targetDB(t)
	_, err := migrate.AutoMSSQL(ctx, dial, build(t), migrate.AutoOptions{Schema: "sales"})
	if err == nil {
		t.Fatal("migrating a schema the unqualified DDL will not land in was allowed")
	}
	if !strings.Contains(err.Error(), "sales") || !strings.Contains(err.Error(), "dbo") {
		t.Errorf("the refusal must name both schemas: %v", err)
	}
}

func msExec(t *testing.T, ctx context.Context, dial migrate.MSSQLDialer, sql string) {
	t.Helper()
	c, closeC, err := dial(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeC()
	if _, err := c.Exec(ctx, sql, nil); err != nil {
		t.Fatalf("%s\n%v", sql, err)
	}
}

func msColumnExists(t *testing.T, ctx context.Context, dial migrate.MSSQLDialer, table, col string) bool {
	t.Helper()
	c, closeC, err := dial(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer closeC()
	s, err := msintro.Introspect(ctx, c, "dbo")
	if err != nil {
		t.Fatal(err)
	}
	tb := s.Table(table)
	return tb != nil && tb.Column(col) != nil
}
