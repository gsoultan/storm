package pg

import (
	"reflect"
	"testing"

	"github.com/gsoultan/storm/schema"
)

// Every definition here is pg_get_indexdef's own output, captured from
// PostgreSQL 17 rather than typed from memory — the reader has to read what
// the server prints, not what the documentation says it prints.
func TestParseIndexDef_ReadsEveryClauseTheServerPrints(t *testing.T) {
	for _, c := range []struct {
		name string
		def  string
		want indexDef
	}{
		{"everything at once",
			`CREATE UNIQUE INDEX ix1 ON ixdef.t USING btree (lower(email) COLLATE "C" text_pattern_ops DESC NULLS LAST, n) INCLUDE (name, ts) NULLS NOT DISTINCT WITH (fillfactor='70') WHERE (deleted_at IS NULL)`,
			indexDef{
				Columns: []schema.IndexColumn{
					{Name: "lower(email)", Expr: true, Collate: "C", OpClass: "text_pattern_ops", Desc: true, NullsLast: true},
					{Name: "n"},
				},
				Include:          []string{"name", "ts"},
				NullsNotDistinct: true,
				With:             []schema.StorageParam{{Name: "fillfactor", Value: "70"}},
				Where:            "deleted_at IS NULL",
			}},
		{"gin opclass",
			`CREATE INDEX ix2 ON ixdef.t USING gin (doc jsonb_path_ops)`,
			indexDef{Columns: []schema.IndexColumn{{Name: "doc", OpClass: "jsonb_path_ops"}}}},
		{"unquoted boolean parameter",
			`CREATE INDEX ix3 ON ixdef.t USING gin (name gin_trgm_ops) WITH (fastupdate=off)`,
			indexDef{Columns: []schema.IndexColumn{{Name: "name", OpClass: "gin_trgm_ops"}},
				With: []schema.StorageParam{{Name: "fastupdate", Value: "off"}}}},
		{"schema-qualified extension opclass is unqualified",
			`CREATE INDEX ix3 ON ixdef.t USING gin (name public.gin_trgm_ops)`,
			indexDef{Columns: []schema.IndexColumn{{Name: "name", OpClass: "gin_trgm_ops"}}}},
		{"whatever schema it is qualified with",
			`CREATE INDEX ix3 ON ixdef.t USING gin (name extensions.gin_trgm_ops)`,
			indexDef{Columns: []schema.IndexColumn{{Name: "name", OpClass: "gin_trgm_ops"}}}},
		{"brin parameter",
			`CREATE INDEX ix4 ON ixdef.t USING brin (ts) WITH (pages_per_range='32')`,
			indexDef{Columns: []schema.IndexColumn{{Name: "ts"}},
				With: []schema.StorageParam{{Name: "pages_per_range", Value: "32"}}}},
		{"only the non-default null placement is printed",
			`CREATE INDEX ix6 ON ixdef.t USING btree (n NULLS FIRST, ts DESC)`,
			indexDef{Columns: []schema.IndexColumn{{Name: "n", NullsFirst: true}, {Name: "ts", Desc: true}}}},
		{"include without the rest",
			`CREATE INDEX ix7 ON ixdef.t USING gist (email gist_trgm_ops) INCLUDE (n)`,
			indexDef{Columns: []schema.IndexColumn{{Name: "email", OpClass: "gist_trgm_ops"}}, Include: []string{"n"}}},
		{"expressions: doubly wrapped arithmetic, bare function call",
			`CREATE INDEX ix8 ON ixdef.t USING btree (((n + 1)), upper(name))`,
			indexDef{Columns: []schema.IndexColumn{{Name: "n + 1", Expr: true}, {Name: "upper(name)", Expr: true}}}},
		{"quoted column",
			`CREATE INDEX ix9 ON ixdef.t USING btree ("Mixed" DESC)`,
			indexDef{Columns: []schema.IndexColumn{{Name: "Mixed", Desc: true}}}},
	} {
		got := parseIndexDef(c.def)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}
