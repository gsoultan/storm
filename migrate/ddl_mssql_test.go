package migrate

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gsoultan/storm/schema"
)

func mtbl(name string, cols ...*schema.Column) *schema.Table {
	return &schema.Table{Name: name, Columns: cols, PrimaryKey: []string{"id"}}
}
func mcol(name, typ string, notNull bool) *schema.Column {
	return &schema.Column{Name: name, Type: schema.Type{Name: typ}, NotNull: notNull}
}
func msch(ts ...*schema.Table) *schema.Schema { return &schema.Schema{Tables: ts} }

func mssqlPlan(t *testing.T, from, to *schema.Schema) Plan {
	t.Helper()
	p, err := DiffFor(from, to, MSSQL)
	if err != nil {
		t.Fatalf("DiffFor: %v", err)
	}
	return p
}

// ADD COLUMN is a syntax error on SQL Server. The word is ADD.
func TestMSSQL_AddColumnHasNoColumnKeyword(t *testing.T) {
	a := msch(mtbl("users", mcol("id", "uuid", true)))
	b := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	got := mssqlPlan(t, a, b).SQL()
	if strings.Contains(got, "ADD COLUMN") {
		t.Errorf("SQL Server has no ADD COLUMN:\n%s", got)
	}
	if !strings.Contains(got, "ALTER TABLE [users] ADD [age]") {
		t.Errorf("want ALTER TABLE [users] ADD [age], got:\n%s", got)
	}
}

// The three column alters are one statement here, and it carries the type even
// when only the nullability moved.
func TestMSSQL_AlterColumnRestatesTypeAndNullability(t *testing.T) {
	a := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	b := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", true)))
	got := mssqlPlan(t, a, b).SQL()
	if strings.Contains(got, "SET NOT NULL") {
		t.Errorf("SQL Server has no SET NOT NULL:\n%s", got)
	}
	if !strings.Contains(got, "ALTER TABLE [users] ALTER COLUMN [age] INT NOT NULL;") {
		t.Errorf("nullability change must restate the type, got:\n%s", got)
	}
}

func TestMSSQL_DropNotNullSpellsNullOut(t *testing.T) {
	a := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", true)))
	b := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	got := mssqlPlan(t, a, b).SQL()
	// Never left implicit: ALTER COLUMN without NULL/NOT NULL means whatever
	// ANSI_NULL_DFLT_ON says, which is a session setting.
	if !strings.Contains(got, "ALTER COLUMN [age] INT NULL;") {
		t.Errorf("want an explicit NULL, got:\n%s", got)
	}
}

// A default is a constraint, so changing one is a drop and an add — and the
// diff still counts it as one change.
func TestMSSQL_SetDefaultReplacesTheConstraint(t *testing.T) {
	old := mcol("state", "int4", true)
	old.Default = "0"
	cur := mcol("state", "int4", true)
	cur.Default = "1"
	a := msch(mtbl("mig_users", mcol("id", "uuid", true), old))
	b := msch(mtbl("mig_users", mcol("id", "uuid", true), cur))

	p := mssqlPlan(t, a, b)
	if len(p.Changes) != 1 {
		t.Fatalf("want one change, got %d:\n%s", len(p.Changes), p.SQL())
	}
	got := p.SQL()
	for _, want := range []string{
		"sys.default_constraints",            // it finds the old name rather than assuming it
		"DROP CONSTRAINT ' + QUOTENAME(",     // and quotes whatever it found
		"EXEC(@storm_df_9_mig_users_state);", // EXEC takes a variable, never a function call
		"ADD CONSTRAINT [DF_mig_users_state] DEFAULT (1) FOR [state];",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// DECLARE is scoped to the batch, so two dropped defaults in one migration
// need two variable names.
func TestMSSQL_TwoDroppedDefaultsDeclareTwoVariables(t *testing.T) {
	withDefaults := func() *schema.Schema {
		x, y := mcol("x", "int4", true), mcol("y", "int4", true)
		x.Default, y.Default = "0", "0"
		return msch(mtbl("users", mcol("id", "uuid", true), x, y))
	}
	b := msch(mtbl("users", mcol("id", "uuid", true), mcol("x", "int4", true), mcol("y", "int4", true)))
	got := mssqlPlan(t, withDefaults(), b).SQL()
	if !strings.Contains(got, "@storm_df_5_users_x") || !strings.Contains(got, "@storm_df_5_users_y") {
		t.Errorf("two drops must declare two variables:\n%s", got)
	}
}

// Sanitising loses information, so the length goes in the name: without it a
// table called `a-b` with a column `c` and a table `a` with a column `b_c`
// declare the same variable, and two DECLAREs of one name in a batch is an
// error halfway through a migration.
func TestMSSQL_DropDefaultVariablesDoNotCollide(t *testing.T) {
	one := dropDefaultSQL(&schema.Table{Name: "a-b"}, mcol("c", "int4", false))
	two := dropDefaultSQL(&schema.Table{Name: "a"}, mcol("b_c", "int4", false))
	if strings.Contains(one, "@storm_df_3_a_b_c") == strings.Contains(two, "@storm_df_3_a_b_c") {
		t.Errorf("two different columns declared the same variable:\n%s\n%s", one, two)
	}
}

func TestMSSQL_DropIndexNamesItsTable(t *testing.T) {
	ix := &schema.Index{Name: "ix_users_age", Columns: []schema.IndexColumn{{Name: "age"}}}
	a := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	a.Tables[0].Indexes = []*schema.Index{ix}
	b := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))

	got := mssqlPlan(t, a, b).SQL()
	if !strings.Contains(got, "DROP INDEX [ix_users_age] ON [users];") {
		t.Errorf("DROP INDEX must name the table, got:\n%s", got)
	}
}

// An un-normalised model is refused rather than mis-diffed: SQL Server has no
// enum, so its columns would each look like a retype and its CHECKs like a
// deletion.
func TestMSSQL_UnnormalisedEnumsAreRefused(t *testing.T) {
	s := msch(mtbl("users", mcol("id", "uuid", true)))
	s.Enums = []*schema.Enum{{Name: "status", Labels: []string{"on", "off"}}}
	_, err := DiffFor(msch(), s, MSSQL)
	if err == nil {
		t.Fatal("a schema declaring enums must not diff against SQL Server")
	}
	if !strings.Contains(err.Error(), "NormalizeMSSQL") {
		t.Errorf("the refusal must say what to do instead, got: %v", err)
	}
}

// A model SQL Server cannot express is a rendering failure, and the failure is
// returned rather than dropped.
func TestMSSQL_RenderFailureReachesTheCaller(t *testing.T) {
	c := mcol("tags", "text", false)
	c.Type.Array = true
	from := msch(mtbl("users", mcol("id", "uuid", true)))
	to := msch(mtbl("users", mcol("id", "uuid", true), c))
	if _, err := DiffFor(from, to, MSSQL); err == nil {
		t.Fatal("an array column must not render as SQL Server DDL")
	}
}

// There is no free non-blocking index build, so the plan comes back unchanged
// instead of gaining an Enterprise-only hint.
func TestMSSQL_ConcurrentlyIsTheIdentity(t *testing.T) {
	live := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	to := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
	to.Tables[0].Indexes = []*schema.Index{{Name: "ix", Columns: []schema.IndexColumn{{Name: "age"}}}}

	p := mssqlPlan(t, live, to)
	got := p.Concurrently(live)
	if got.SQL() != p.SQL() {
		t.Errorf("Concurrently changed a SQL Server plan:\n%s\nwant:\n%s", got.SQL(), p.SQL())
	}
	for _, c := range got.Changes {
		if c.NoTransaction {
			t.Error("nothing in a SQL Server plan needs to run outside a transaction")
		}
	}
}

func TestDialectIsRefusedByName(t *testing.T) {
	if _, err := DiffFor(msch(), msch(), Dialect("oracle")); err == nil {
		t.Fatal("an unknown dialect must not silently render as PostgreSQL")
	}
}

// Diff drops the error DiffFor returns, which is only honest while every
// PostgreSQL renderer returns a literal nil. This holds that in place: a new
// renderer that can fail has to either be infallible or make Diff say so.
func TestPostgresRenderersCannotFail(t *testing.T) {
	d := postgresDDL()
	tbl := &schema.Table{
		Name:       "users",
		Columns:    []*schema.Column{{Name: "id", Type: schema.Type{Name: "uuid"}, NotNull: true}},
		PrimaryKey: []string{"id"},
	}
	v := reflect.ValueOf(d)
	rt := v.Type()
	errType := reflect.TypeOf((*error)(nil)).Elem()

	for i := 0; i < rt.NumField(); i++ {
		f, ft := v.Field(i), rt.Field(i)
		if f.IsNil() {
			continue
		}
		fn := f.Type()
		if fn.NumOut() == 0 || !fn.Out(fn.NumOut()-1).Implements(errType) {
			continue // cannot fail by signature
		}

		var args []reflect.Value
		switch {
		case ft.Name == "Enums":
			args = []reflect.Value{
				reflect.ValueOf(&Plan{}),
				reflect.ValueOf(&schema.Schema{}),
				reflect.ValueOf(&schema.Schema{Tables: []*schema.Table{tbl}}),
			}
		case fn.NumIn() == 1 && fn.In(0) == reflect.TypeOf(tbl):
			args = []reflect.Value{reflect.ValueOf(tbl)}
		case fn.NumIn() == 2 && fn.In(0) == reflect.TypeOf(tbl) && fn.In(1) == reflect.TypeOf(tbl.Columns[0]):
			args = []reflect.Value{reflect.ValueOf(tbl), reflect.ValueOf(tbl.Columns[0])}
		default:
			t.Fatalf("postgresDDL.%s can fail and this test does not know how to call it: "+
				"teach it the argument shape, or check whether Diff can still drop the error", ft.Name)
		}

		out := f.Call(args)
		if e := out[len(out)-1].Interface(); e != nil {
			t.Errorf("postgresDDL.%s returned %v; Diff drops that error", ft.Name, e)
		}
	}
}

func TestSplitBatchesKeepsASemicolonInsideALiteral(t *testing.T) {
	got := splitBatches("CREATE TABLE [a;b] (x int CHECK (x IN ('a;b', 'c''d')));\nCREATE TABLE [c] (y int);")
	if len(got) != 2 {
		t.Fatalf("want 2 statements, got %d: %q", len(got), got)
	}
	if !strings.Contains(got[0], "'a;b'") || !strings.Contains(got[0], "[a;b]") {
		t.Errorf("a semicolon in a literal or an identifier is not a statement end: %q", got[0])
	}
}

// A table created by a plan arrives with its indexes.
//
// The seam's two back ends implement this in different places — pgddl.CreateTable
// appends them, msddl.Create emits them in a second pass over the whole schema
// that a diff building one table at a time never reaches — so the property is
// pinned here rather than in either of them. Without it a new table lands
// unindexed and the NEXT diff proposes to add what the first one should have.
func TestANewTableArrivesWithItsIndexes(t *testing.T) {
	for _, dialect := range []Dialect{Postgres, MSSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			to := msch(mtbl("users", mcol("id", "uuid", true), mcol("age", "int4", false)))
			to.Tables[0].Indexes = []*schema.Index{
				{Name: "ix_users_age", Columns: []schema.IndexColumn{{Name: "age"}}},
			}
			p, err := DiffFor(msch(), to, dialect)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(p.SQL(), "ix_users_age") {
				t.Errorf("a new table lost its index:\n%s", p.SQL())
			}
		})
	}
}

// The migration lock tells "somebody else has it" from "this did not work" by
// RAISERROR STATE, because every ad-hoc RAISERROR is error 50000 and msdrv
// drops the message text on purpose. The assertion is on an interface so this
// package links no driver — which is exactly the thing a refactor would undo
// without noticing, so it is pinned here.
type fakeServerErr struct{ state uint8 }

func (fakeServerErr) Error() string        { return "server error 50000" }
func (e fakeServerErr) ServerState() uint8 { return e.state }

func TestApplockBusyIsToldApartByState(t *testing.T) {
	if !isApplockBusy(fakeServerErr{state: applockBusyState}) {
		t.Error("the state lockMSSQL raises was not recognised as contention")
	}
	if isApplockBusy(fakeServerErr{state: 1}) {
		t.Error("state 1 — what every other RAISERROR uses — was read as contention")
	}
	if isApplockBusy(errors.New("connection refused")) {
		t.Error("an error with no state at all was read as contention")
	}
	// Wrapped, because that is how it arrives: msdrv's error comes back inside
	// whatever Exec wrapped it in.
	if !isApplockBusy(fmt.Errorf("exec: %w", fakeServerErr{state: applockBusyState})) {
		t.Error("a wrapped server error was not unwrapped")
	}
}

func TestMSSQLSchemaNamesAreValidatedForSQLServerAndNotPostgres(t *testing.T) {
	// Uppercase is legal here and is not in validIdent, which is PostgreSQL's.
	if err := validMSSQLIdent("Sales"); err != nil {
		t.Errorf("a mixed-case schema name is legal on SQL Server: %v", err)
	}
	for _, bad := range []string{"", "1sales", "sales;drop", "sa les", strings.Repeat("s", 129)} {
		if err := validMSSQLIdent(bad); err == nil {
			t.Errorf("%q was accepted as a schema name", bad)
		}
	}
}

func TestAutoMSSQLRefusesConcurrentlyByName(t *testing.T) {
	_, err := AutoMSSQL(context.Background(), nil, msch(), AutoOptions{Concurrently: true})
	if err == nil {
		t.Fatal("Concurrently was accepted for a target with no concurrent index build")
	}
	if !strings.Contains(err.Error(), "ONLINE = ON") {
		t.Errorf("the refusal must name what SQL Server has instead: %v", err)
	}
}
