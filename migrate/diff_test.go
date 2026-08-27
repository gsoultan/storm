package migrate_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/schema"
)

func tbl(name string, cols ...*schema.Column) *schema.Table {
	return &schema.Table{Name: name, Columns: cols, PrimaryKey: []string{"id"}}
}
func col(name, typ string, notNull bool) *schema.Column {
	return &schema.Column{Name: name, Type: schema.Type{Name: typ}, NotNull: notNull}
}
func sch(ts ...*schema.Table) *schema.Schema { return &schema.Schema{Tables: ts} }

func TestDiff_NoChange(t *testing.T) {
	a := sch(tbl("users", col("id", "uuid", true), col("email", "text", true)))
	b := sch(tbl("users", col("id", "uuid", true), col("email", "text", true)))
	if p := migrate.Diff(a, b); !p.Empty() {
		t.Fatalf("identical schemas produced a plan:\n%s", p.SQL())
	}
}

func TestDiff_Idempotent(t *testing.T) {
	// Applying a plan and re-diffing must produce nothing. Modelled here by
	// diffing to itself after the first diff normalised both sides.
	a := sch()
	b := sch(tbl("users", col("id", "uuid", true), col("email", "text", true)))
	if migrate.Diff(a, b).Empty() {
		t.Fatal("creating a table produced no plan")
	}
	if p := migrate.Diff(b, b); !p.Empty() {
		t.Fatalf("re-diff after apply is not empty:\n%s", p.SQL())
	}
}

func TestDiff_AddColumn(t *testing.T) {
	a := sch(tbl("users", col("id", "uuid", true)))
	b := sch(tbl("users", col("id", "uuid", true), col("age", "int4", false)))
	p := migrate.Diff(a, b)
	if len(p.Changes) != 1 || !strings.Contains(p.Changes[0].SQL, "ADD COLUMN") {
		t.Fatalf("want one ADD COLUMN, got:\n%s", p.SQL())
	}
	if p.Destructive() {
		t.Error("adding a nullable column must not be destructive")
	}
}

func TestDiff_AddNotNullColumnIsDestructive(t *testing.T) {
	a := sch(tbl("users", col("id", "uuid", true)))
	b := sch(tbl("users", col("id", "uuid", true), col("age", "int4", true)))
	p := migrate.Diff(a, b)
	if !p.Destructive() {
		t.Fatalf("NOT NULL with no default must be flagged:\n%s", p.SQL())
	}
	if !strings.Contains(p.SQL(), "-- storm:destructive") {
		t.Error("destructive steps must be annotated in the SQL")
	}
}

func TestDiff_AddNotNullWithDefaultIsSafe(t *testing.T) {
	a := sch(tbl("users", col("id", "uuid", true)))
	c := col("age", "int4", true)
	c.Default = "0"
	b := sch(tbl("users", col("id", "uuid", true), c))
	if migrate.Diff(a, b).Destructive() {
		t.Error("NOT NULL with a default is safe")
	}
}

func TestDiff_DropIsDestructive(t *testing.T) {
	a := sch(tbl("users", col("id", "uuid", true), col("email", "text", true)))
	b := sch(tbl("users", col("id", "uuid", true)))
	p := migrate.Diff(a, b)
	if !p.Destructive() || !strings.Contains(p.SQL(), "DROP COLUMN") {
		t.Fatalf("want a destructive DROP COLUMN, got:\n%s", p.SQL())
	}
}

func TestDiff_NarrowingIsDestructive(t *testing.T) {
	wide := &schema.Column{Name: "name", Type: schema.Type{Name: "varchar", Size: 200}, NotNull: true}
	narrow := &schema.Column{Name: "name", Type: schema.Type{Name: "varchar", Size: 50}, NotNull: true}
	a := sch(tbl("users", col("id", "uuid", true), wide))
	b := sch(tbl("users", col("id", "uuid", true), narrow))
	if !migrate.Diff(a, b).Destructive() {
		t.Error("varchar(200) -> varchar(50) can truncate")
	}
	// Widening is safe.
	if migrate.Diff(b, a).Destructive() {
		t.Error("varchar(50) -> varchar(200) is safe")
	}
}

func TestDiff_ExpressionsCompareCanonically(t *testing.T) {
	// Postgres rewrites what it stores; whitespace and outer parens must not
	// register as a change.
	mk := func(expr string) *schema.Schema {
		tt := tbl("users", col("id", "uuid", true))
		tt.Checks = []*schema.Check{{Name: "ck_age", Expr: expr}}
		return sch(tt)
	}
	if p := migrate.Diff(mk("age > 0"), mk("((age  >  0))")); !p.Empty() {
		t.Fatalf("whitespace/parens must not diff:\n%s", p.SQL())
	}
}

func TestDiff_EnumLabelAppend(t *testing.T) {
	a := &schema.Schema{Enums: []*schema.Enum{{Name: "status", Labels: []string{"a"}}}}
	b := &schema.Schema{Enums: []*schema.Enum{{Name: "status", Labels: []string{"a", "b"}}}}
	p := migrate.Diff(a, b)
	if !strings.Contains(p.SQL(), "ADD VALUE 'b'") {
		t.Fatalf("want ALTER TYPE ADD VALUE, got:\n%s", p.SQL())
	}
	if p.Destructive() {
		t.Error("appending an enum label is safe")
	}
	// Removing one is not.
	if !migrate.Diff(b, a).Destructive() {
		t.Error("removing an enum label must be flagged")
	}
}
