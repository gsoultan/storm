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

func withIndex(ix *schema.Index) *schema.Schema {
	t := tbl("docs", col("id", "uuid", true), col("slug", "text", true), col("title", "text", false))
	t.Indexes = []*schema.Index{ix}
	return sch(t)
}

// Every index fact the model can state has to move the differ, or a changed
// operator class — the difference between an index that serves LIKE 'abc%'
// and one that does not — would never reach the database.
func TestDiff_IndexFactsAreCompared(t *testing.T) {
	base := func() *schema.Index {
		return &schema.Index{Columns: []schema.IndexColumn{{Name: "slug"}}}
	}
	for _, c := range []struct {
		name   string
		change func(*schema.Index)
	}{
		{"opclass", func(ix *schema.Index) { ix.Columns[0].OpClass = "text_pattern_ops" }},
		{"collation", func(ix *schema.Index) { ix.Columns[0].Collate = "C" }},
		{"nulls first", func(ix *schema.Index) { ix.Columns[0].NullsFirst = true }},
		{"include", func(ix *schema.Index) { ix.Include = []string{"title"} }},
		{"nulls not distinct", func(ix *schema.Index) { ix.Unique, ix.NullsNotDistinct = true, true }},
		{"storage parameter", func(ix *schema.Index) { ix.With = []schema.StorageParam{{Name: "fillfactor", Value: "70"}} }},
		{"storage parameter value", func(ix *schema.Index) { ix.With = []schema.StorageParam{{Name: "fillfactor", Value: "90"}} }},
	} {
		from, to := withIndex(base()), withIndex(base())
		if c.name == "storage parameter value" {
			from.Tables[0].Indexes[0].With = []schema.StorageParam{{Name: "fillfactor", Value: "70"}}
		}
		c.change(to.Tables[0].Indexes[0])
		p := migrate.Diff(from, to)
		if p.Empty() {
			t.Errorf("%s: changing it produced no plan", c.name)
			continue
		}
		sql := p.SQL()
		if !strings.Contains(sql, "DROP INDEX") || !strings.Contains(sql, "CREATE INDEX") &&
			!strings.Contains(sql, "CREATE UNIQUE INDEX") {
			t.Errorf("%s: want drop and recreate, got:\n%s", c.name, sql)
		}
		// And the same fact on both sides is not a change.
		again := withIndex(base())
		c.change(again.Tables[0].Indexes[0])
		if p := migrate.Diff(to, again); !p.Empty() {
			t.Errorf("%s: identical indexes produced a plan:\n%s", c.name, p.SQL())
		}
	}
}

// Concurrently rewrites index work on LIVE tables only. An index on a table
// the same plan creates stays a plain CREATE INDEX: nothing writes to a table
// that does not exist yet, and the concurrent form could not share the
// transaction that creates the table anyway.
func TestPlan_ConcurrentlyRewritesIndexesOnLiveTablesOnly(t *testing.T) {
	live := sch(tbl("docs", col("id", "uuid", true), col("slug", "text", true)))
	want := sch(
		tbl("docs", col("id", "uuid", true), col("slug", "text", true)),
		tbl("posts", col("id", "uuid", true), col("title", "text", true)),
	)
	want.Tables[0].Indexes = []*schema.Index{{Columns: []schema.IndexColumn{{Name: "slug"}}}}
	want.Tables[1].Indexes = []*schema.Index{{Columns: []schema.IndexColumn{{Name: "title"}}}}

	plain := migrate.Diff(live, want)
	if strings.Contains(plain.SQL(), "CONCURRENTLY") {
		t.Fatalf("a plain plan builds concurrently:\n%s", plain.SQL())
	}
	p := migrate.Diff(live, want).Concurrently(live)
	sql := p.SQL()
	if !strings.Contains(sql, `CREATE INDEX CONCURRENTLY "ix_docs_slug" ON "docs"`) {
		t.Errorf("the index on the live table is not concurrent:\n%s", sql)
	}
	if strings.Contains(sql, `CONCURRENTLY "ix_posts_title"`) {
		t.Errorf("the index on the new table was made concurrent:\n%s", sql)
	}
	// The marker rides above the statement, and only that one.
	n := 0
	for _, c := range p.Changes {
		if c.NoTransaction {
			n++
			if !strings.Contains(c.SQL, "CONCURRENTLY") {
				t.Errorf("marked no-transaction without being concurrent: %s", c.SQL)
			}
		}
	}
	if n != 1 {
		t.Errorf("want exactly 1 no-transaction change, got %d", n)
	}
	if strings.Count(sql, migrate.NoTransactionMarker) != 1 {
		t.Errorf("the marker appears %d times:\n%s", strings.Count(sql, migrate.NoTransactionMarker), sql)
	}
}

// Dropping is the other half: an index that leaves the model, or is recreated
// with a new definition, is dropped CONCURRENTLY too.
func TestPlan_ConcurrentlyDropsConcurrently(t *testing.T) {
	from := withIndex(&schema.Index{Columns: []schema.IndexColumn{{Name: "slug"}}})
	to := withIndex(&schema.Index{Columns: []schema.IndexColumn{{Name: "slug", OpClass: "text_pattern_ops"}}})
	p := migrate.Diff(from, to).Concurrently(from)
	sql := p.SQL()
	for _, want := range []string{`DROP INDEX CONCURRENTLY "ix_docs_slug"`, `CREATE INDEX CONCURRENTLY "ix_docs_slug"`} {
		if !strings.Contains(sql, want) {
			t.Errorf("want %s in:\n%s", want, sql)
		}
	}
}
