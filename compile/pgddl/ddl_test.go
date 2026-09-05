package pgddl_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/schema"
)

func tbl() *schema.Table {
	return &schema.Table{Name: "docs", Columns: []*schema.Column{
		{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
		{Name: "slug", Type: schema.Type{Name: schema.TypeText}, NotNull: true},
		{Name: "title", Type: schema.Type{Name: schema.TypeText}},
		{Name: "score", Type: schema.Type{Name: schema.TypeInt4}},
	}, PrimaryKey: []string{"id"}}
}

// Every clause, in the order pg_get_indexdef prints them — because the round
// trip is a fixpoint only if the emitter and the reader agree on the shape.
func TestCreateIndex_RendersEveryClause(t *testing.T) {
	for _, c := range []struct {
		name string
		ix   *schema.Index
		want string
	}{
		{"plain", &schema.Index{Name: "i", Columns: []schema.IndexColumn{{Name: "slug"}}},
			`CREATE INDEX "i" ON "docs" ("slug");`},
		{"unique desc nulls last", &schema.Index{Name: "i", Unique: true,
			Columns: []schema.IndexColumn{{Name: "score", Desc: true, NullsLast: true}}},
			`CREATE UNIQUE INDEX "i" ON "docs" ("score" DESC NULLS LAST);`},
		{"asc nulls first", &schema.Index{Name: "i",
			Columns: []schema.IndexColumn{{Name: "score", NullsFirst: true}}},
			`CREATE INDEX "i" ON "docs" ("score" NULLS FIRST);`},
		{"collate then opclass", &schema.Index{Name: "i",
			Columns: []schema.IndexColumn{{Name: "slug", Collate: "C", OpClass: "text_pattern_ops"}}},
			`CREATE INDEX "i" ON "docs" ("slug" COLLATE "C" text_pattern_ops);`},
		{"expression is parenthesised", &schema.Index{Name: "i",
			Columns: []schema.IndexColumn{{Name: "score + 1", Expr: true}, {Name: "lower(slug)", Expr: true}}},
			`CREATE INDEX "i" ON "docs" ((score + 1), (lower(slug)));`},
		{"method", &schema.Index{Name: "i", Method: "gin",
			Columns: []schema.IndexColumn{{Name: "title", OpClass: "gin_trgm_ops"}}},
			`CREATE INDEX "i" ON "docs" USING gin ("title" public.gin_trgm_ops);`},
		{"a qualified opclass is the model's own", &schema.Index{Name: "i", Method: "gin",
			Columns: []schema.IndexColumn{{Name: "title", OpClass: "extensions.gin_trgm_ops"}}},
			`CREATE INDEX "i" ON "docs" USING gin ("title" extensions.gin_trgm_ops);`},
		{"btree is the default and not spelled", &schema.Index{Name: "i", Method: "btree",
			Columns: []schema.IndexColumn{{Name: "slug"}}},
			`CREATE INDEX "i" ON "docs" ("slug");`},
		{"include, nulls not distinct, with, where", &schema.Index{Name: "i", Unique: true,
			Columns:          []schema.IndexColumn{{Name: "slug"}},
			Include:          []string{"title", "score"},
			NullsNotDistinct: true,
			With:             []schema.StorageParam{{Name: "fillfactor", Value: "70"}, {Name: "deduplicate_items", Value: "off"}},
			Where:            "score > 0"},
			`CREATE UNIQUE INDEX "i" ON "docs" ("slug") INCLUDE ("title", "score") NULLS NOT DISTINCT WITH (fillfactor = 70, deduplicate_items = off) WHERE score > 0;`},
	} {
		if got := pgddl.CreateIndex(tbl(), c.ix); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// A trigram operator class needs pg_trgm, and the DDL has to install it
// before the CREATE INDEX that needs it — wrapped, because IF NOT EXISTS
// races with itself under two appliers.
func TestCreate_InstallsPgTrgmWhenAnIndexNeedsIt(t *testing.T) {
	s := &schema.Schema{Tables: []*schema.Table{tbl()}}
	if pgddl.NeedsPgTrgm(s) {
		t.Fatal("a schema with no trigram index claims to need pg_trgm")
	}
	if strings.Contains(pgddl.Create(s), "pg_trgm") {
		t.Fatal("pg_trgm installed for a schema that does not use it")
	}
	s.Tables[0].Indexes = []*schema.Index{{Name: "i", Method: "gin",
		Columns: []schema.IndexColumn{{Name: "title", OpClass: "gin_trgm_ops"}}}}
	if !pgddl.NeedsPgTrgm(s) {
		t.Fatal("gin_trgm_ops does not need pg_trgm?")
	}
	ddl := pgddl.Create(s)
	ext, ix := strings.Index(ddl, "pg_trgm WITH SCHEMA public"), strings.Index(ddl, "gin_trgm_ops")
	if ext < 0 || ix < 0 || ext > ix {
		t.Fatalf("extension must be installed before the index that needs it:\n%s", ddl)
	}
	if !strings.Contains(ddl, "EXCEPTION WHEN unique_violation OR duplicate_object") {
		t.Fatalf("the install is not guarded against a concurrent applier:\n%s", ddl)
	}
}

// What the model can say for MySQL's sake and PostgreSQL cannot do. Dropping
// a prefix length silently would index the whole column — a different index
// with a different cost — so it is refused, naming the alternative.
func TestCheck_RefusesMySQLOnlyIndexFacts(t *testing.T) {
	for _, c := range []struct {
		name string
		ix   *schema.Index
		want []string
	}{
		{"prefix", &schema.Index{Name: "i", Columns: []schema.IndexColumn{{Name: "slug", Prefix: 16}}},
			[]string{"16-character prefix", "slug", "left(%s, 16)"}},
		{"invisible", &schema.Index{Name: "i", Invisible: true, Columns: []schema.IndexColumn{{Name: "slug"}}},
			[]string{"INVISIBLE"}},
		{"fulltext", &schema.Index{Name: "i", Method: "fulltext", Columns: []schema.IndexColumn{{Name: "title"}}},
			[]string{"FULLTEXT", "tsvector"}},
	} {
		s := &schema.Schema{Tables: []*schema.Table{tbl()}}
		s.Tables[0].Indexes = []*schema.Index{c.ix}
		err := pgddl.Check(s)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error does not mention %q:\n%v", c.name, w, err)
			}
		}
	}
	s := &schema.Schema{Tables: []*schema.Table{tbl()}}
	s.Tables[0].Indexes = []*schema.Index{{Name: "i", Columns: []schema.IndexColumn{{Name: "slug"}}}}
	if err := pgddl.Check(s); err != nil {
		t.Fatalf("a plain index was refused: %v", err)
	}
}
