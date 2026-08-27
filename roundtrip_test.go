package storm_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/internal/testmodel"
	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/schema"
	pgintro "github.com/gsoultan/storm/schema/pg"
	"github.com/jackc/pgx/v5"
)

func dsn(t testing.TB) string {
	d := os.Getenv("STORM_DSN")
	if d == "" {
		t.Skip("STORM_DSN unset")
	}
	return d
}

func connect(t testing.TB) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

// applyInto creates a scratch namespace, applies ddl inside it, and reads it
// back into the IR.
func applyInto(t *testing.T, c *pgx.Conn, ns, ddl string) *schema.Schema {
	t.Helper()
	ctx := context.Background()
	// btree_gist is what lets an exclusion constraint mix `=` on a scalar with
	if _, err := c.Exec(ctx, `DROP SCHEMA IF EXISTS `+ns+` CASCADE; CREATE SCHEMA `+ns); err != nil {
		t.Fatalf("create schema %s: %v", ns, err)
	}
	if _, err := c.Exec(ctx, `SET search_path TO `+ns); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, ddl); err != nil {
		t.Fatalf("apply DDL into %s: %v\n---\n%s", ns, err, numbered(ddl))
	}
	got, err := pgintro.Introspect(ctx, c, ns)
	if err != nil {
		t.Fatalf("introspect %s: %v", ns, err)
	}
	return got
}

// TestRoundTrip is M1's exit gate.
//
// Comparing the model IR to an introspected IR directly would report false
// differences, because Postgres rewrites every expression it stores: the CHECK
// `age BETWEEN 0 AND 150` comes back as `age >= 0 AND age <= 150`. So the gate
// is a *fixpoint*: emit, apply, introspect, emit again, apply again, introspect
// again. If those two IRs differ, either the emitter or the reader is lossy.
func TestRoundTrip(t *testing.T) {
	c := connect(t)

	s, err := storm.Build(testmodel.All()...)
	if err != nil {
		t.Fatal(err)
	}
	ddl1 := pgddl.Create(s)

	got1 := applyInto(t, c, "rt1", ddl1)
	ddl2 := pgddl.Create(got1)
	got2 := applyInto(t, c, "rt2", ddl2)

	ddl3 := pgddl.Create(got2)
	if ddl2 != ddl3 {
		t.Errorf("emit is not a fixpoint — introspection or emission is lossy:\n%s",
			firstDiff(ddl2, ddl3))
	}

	// And the structural facts must survive from the model itself.
	compareStructure(t, s, got1)
}

// compareStructure checks everything Postgres does NOT rewrite: table set,
// column names, types, nullability, keys, and referential actions.
func compareStructure(t *testing.T, want, got *schema.Schema) {
	t.Helper()
	if len(want.Tables) != len(got.Tables) {
		t.Fatalf("table count: model %d, database %d", len(want.Tables), len(got.Tables))
	}
	for _, wt := range want.Tables {
		gt := got.Table(wt.Name)
		if gt == nil {
			t.Errorf("table %s missing from database", wt.Name)
			continue
		}
		if len(wt.Columns) != len(gt.Columns) {
			t.Errorf("%s: column count model %d, database %d", wt.Name, len(wt.Columns), len(gt.Columns))
		}
		for i, wc := range wt.Columns {
			gc := gt.Column(wc.Name)
			if gc == nil {
				t.Errorf("%s.%s missing from database", wt.Name, wc.Name)
				continue
			}
			if i < len(gt.Columns) && gt.Columns[i].Name != wc.Name {
				t.Errorf("%s: column %d is %q in the model but %q in the database (order is observable)",
					wt.Name, i, wc.Name, gt.Columns[i].Name)
			}
			if !wc.Type.Equal(gc.Type) {
				t.Errorf("%s.%s type: model %s, database %s", wt.Name, wc.Name, wc.Type.SQL(), gc.Type.SQL())
			}
			if wc.NotNull != gc.NotNull {
				t.Errorf("%s.%s NOT NULL: model %v, database %v", wt.Name, wc.Name, wc.NotNull, gc.NotNull)
			}
		}
		if strings.Join(wt.PrimaryKey, ",") != strings.Join(gt.PrimaryKey, ",") {
			t.Errorf("%s primary key: model %v, database %v", wt.Name, wt.PrimaryKey, gt.PrimaryKey)
		}
		if len(wt.ForeignKeys) != len(gt.ForeignKeys) {
			t.Errorf("%s foreign keys: model %d, database %d", wt.Name, len(wt.ForeignKeys), len(gt.ForeignKeys))
		}
		for _, wfk := range wt.ForeignKeys {
			var gfk *schema.ForeignKey
			for _, f := range gt.ForeignKeys {
				if f.Name == wfk.Name {
					gfk = f
					break
				}
			}
			if gfk == nil {
				t.Errorf("%s: foreign key %s missing", wt.Name, wfk.Name)
				continue
			}
			if wfk.RefTable != gfk.RefTable || wfk.OnDelete != gfk.OnDelete {
				t.Errorf("%s.%s: model -> %s ON DELETE %q, database -> %s ON DELETE %q",
					wt.Name, wfk.Name, wfk.RefTable, wfk.OnDelete, gfk.RefTable, gfk.OnDelete)
			}
		}
		if len(wt.Indexes) != len(gt.Indexes) {
			t.Errorf("%s indexes: model %d %v, database %d %v",
				wt.Name, len(wt.Indexes), names(wt.Indexes), len(gt.Indexes), names(gt.Indexes))
		}
		if len(wt.Excludes) != len(gt.Excludes) {
			t.Errorf("%s exclusion constraints: model %d, database %d", wt.Name, len(wt.Excludes), len(gt.Excludes))
		}
	}
	if len(want.Enums) != len(got.Enums) {
		t.Errorf("enum count: model %d, database %d", len(want.Enums), len(got.Enums))
	}
	for _, we := range want.Enums {
		ge := got.Enum(we.Name)
		if ge == nil {
			t.Errorf("enum %s missing", we.Name)
			continue
		}
		if strings.Join(we.Labels, ",") != strings.Join(ge.Labels, ",") {
			t.Errorf("enum %s labels: model %v, database %v", we.Name, we.Labels, ge.Labels)
		}
	}
}

func names(ix []*schema.Index) []string {
	out := make([]string, len(ix))
	for i, x := range ix {
		out[i] = x.Name
	}
	return out
}

func firstDiff(a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(la) && i < len(lb); i++ {
		if la[i] != lb[i] {
			return "line " + itoa(i+1) + ":\n  first:  " + la[i] + "\n  second: " + lb[i]
		}
	}
	return "length " + itoa(len(la)) + " vs " + itoa(len(lb))
}

func numbered(s string) string {
	var b strings.Builder
	for i, l := range strings.Split(s, "\n") {
		b.WriteString(itoa(i+1) + "\t" + l + "\n")
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestMigrateConverges is the differ's real gate: a plan generated against a
// live database must, once applied, leave nothing to do. A differ that emits an
// incomplete plan looks fine in unit tests and corrupts a production schema.
func TestMigrateConverges(t *testing.T) {
	c := connect(t)
	ctx := context.Background()

	want, err := storm.Build(testmodel.All()...)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Exec(ctx, `DROP SCHEMA IF EXISTS mig CASCADE; CREATE SCHEMA mig; SET search_path TO mig`); err != nil {
		t.Fatal(err)
	}

	empty, err := pgintro.Introspect(ctx, c, "mig")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := migrate.For(ctx, c, "mig", want)
	if err != nil {
		t.Fatal(err)
	}
	_ = empty
	if plan.Empty() {
		t.Fatal("diff against an empty database produced no plan")
	}
	if _, err := c.Exec(ctx, plan.SQL()); err != nil {
		t.Fatalf("apply plan: %v\n---\n%s", err, plan.SQL())
	}

	// Converged?
	if _, err := c.Exec(ctx, "SET search_path TO mig"); err != nil {
		t.Fatal(err)
	}
	p2, err := migrate.For(ctx, c, "mig", want)
	if err != nil {
		t.Fatal(err)
	}
	if p := p2; !p.Empty() {
		t.Errorf("schema did not converge — %d step(s) still pending after applying the plan:\n%s",
			len(p.Changes), p.SQL())
	}

	// Evolve: remove a table from the model and check the drop is both
	// generated and flagged destructive.
	after, err := pgintro.Introspect(ctx, c, "mig")
	if err != nil {
		t.Fatal(err)
	}
	reduced := &schema.Schema{Enums: want.Enums}
	for _, tb := range want.Tables {
		if tb.Name != "bookings" {
			reduced.Tables = append(reduced.Tables, tb)
		}
	}
	drop, err := migrate.For(ctx, c, "mig", reduced)
	if err != nil {
		t.Fatal(err)
	}
	_ = after
	if !drop.Destructive() {
		t.Error("dropping a table must be flagged destructive")
	}
	if !strings.Contains(drop.SQL(), `DROP TABLE "bookings"`) {
		t.Errorf("want DROP TABLE bookings, got:\n%s", drop.SQL())
	}
	if _, err := c.Exec(ctx, drop.SQL()); err != nil {
		t.Fatalf("apply drop: %v\n%s", err, drop.SQL())
	}
	if _, err := c.Exec(ctx, "SET search_path TO mig"); err != nil {
		t.Fatal(err)
	}
	p3, err := migrate.For(ctx, c, "mig", reduced)
	if err != nil {
		t.Fatal(err)
	}
	if p := p3; !p.Empty() {
		t.Errorf("did not converge after the drop:\n%s", p.SQL())
	}
}

func dump(s *schema.Schema) string {
	var b strings.Builder
	for _, e := range s.Enums {
		b.WriteString("enum " + e.Name + " " + strings.Join(e.Labels, ",") + "\n")
	}
	for _, t := range s.Tables {
		b.WriteString("table " + t.Name + "\n")
		for _, c := range t.Columns {
			b.WriteString("  col " + c.Name + " " + c.Type.SQL() + "\n")
		}
		for _, i := range t.Indexes {
			b.WriteString("  idx " + i.Name + "\n")
		}
		for _, f := range t.ForeignKeys {
			b.WriteString("  fk " + f.Name + " -> " + f.RefTable + "\n")
		}
	}
	return b.String()
}
