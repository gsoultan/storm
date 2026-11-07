package storm_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/mssql"
	"github.com/gsoultan/storm/compile/mysql"
	"github.com/gsoultan/storm/compile/oracle"
	"github.com/gsoultan/storm/compile/pgsql"
)

type litrow struct {
	storm.Model
	Status string
	Amount storm.Decimal
	Score  float64
	At     time.Time
	Ok     bool
}

var litAt = time.Date(2026, 1, 2, 3, 4, 5, 600_000_000, time.FixedZone("x", 3600))

func (m *litrow) Aggregates(a *storm.Aggregates) {
	b := a.Named("Lits")
	b.By(&m.Status)
	b.Max(a.Coalesce(&m.At, a.Lit(litAt)), "T")
	b.Sum(a.NullIf(&m.Amount, a.Lit(storm.Decimal{Unscaled: 150, Scale: 2})), "N")
	b.Max(a.Coalesce(&m.Status, a.Lit("it's")), "S")
	b.Max(a.Coalesce(&m.Score, a.Lit(1.5)), "F")
}

// A declared literal renders in each dialect's own spelling.
//
// Root cause of what this pins: every SQL back end wrote schema.Literal.SQL,
// which is PostgreSQL's — and which had no case for a timestamp at all, so
// Lit(time.Time) rendered as NULL. `at >= NULL` is never true: a filtered
// count over a time literal silently counted nothing, in every dialect. The
// others got `'1.50'::numeric` and `true`, syntax errors outside PostgreSQL.
func TestLiteralsRenderPerDialect(t *testing.T) {
	s, err := storm.Build(&litrow{})
	if err != nil {
		t.Fatal(err)
	}
	agg := s.Table("litrows").Aggregates[0]

	pg := pgsql.AggregateSelect("litrows", agg)
	for _, want := range []string{
		`coalesce("at", '2026-01-02T02:04:05.6Z'::timestamptz)`,
		`nullif("amount", '1.50'::numeric)`,
		`coalesce("status", 'it''s')`,
		`coalesce("score", 1.5)`,
	} {
		if !strings.Contains(pg, want) {
			t.Errorf("postgres: want %s in\n%s", want, pg)
		}
	}

	type lowered struct {
		name string
		sql  string
		err  error
		want []string
	}
	my, myErr := mysql.AggregateSelect("litrows", agg)
	ms, msErr := mssql.AggregateSelect("litrows", agg)
	ora, oraErr := oracle.AggregateSelect("litrows", agg)
	for _, c := range []lowered{
		{"mysql", my, myErr, []string{
			"coalesce(`at`, TIMESTAMP'2026-01-02 02:04:05.600000')",
			"nullif(`amount`, 1.50)", "coalesce(`status`, 'it''s')", "coalesce(`score`, 1.5e+00)",
		}},
		{"mssql", ms, msErr, []string{
			"coalesce([at], CAST('2026-01-02T02:04:05.6000000+00:00' AS DATETIMEOFFSET(7)))",
			"nullif([amount], 1.50)", "coalesce([status], N'it''s')", "coalesce([score], 1.5e+00)",
		}},
		{"oracle", ora, oraErr, []string{
			`coalesce("at", TIMESTAMP '2026-01-02 02:04:05.600000 +00:00')`,
			`nullif("amount", 1.50)`, `coalesce("status", 'it''s')`, `coalesce("score", 1.5e+00D)`,
		}},
	} {
		if c.err != nil {
			t.Errorf("%s: %v", c.name, c.err)
			continue
		}
		if strings.Contains(c.sql, "::") || strings.Contains(c.sql, "NULL)") {
			t.Errorf("%s: PostgreSQL spelling or a NULL literal leaked:\n%s", c.name, c.sql)
		}
		for _, w := range c.want {
			if !strings.Contains(c.sql, w) {
				t.Errorf("%s: want %s in\n%s", c.name, w, c.sql)
			}
		}
	}
}

type litbad struct {
	storm.Model
	Status string
	Score  float64
}

func (m *litbad) Aggregates(a *storm.Aggregates) {
	b := a.Named("Bad")
	b.By(&m.Status)
	b.Max(a.Coalesce(&m.Status, a.Lit(`a\'b`)), "S")
	b.Max(a.Coalesce(&m.Score, a.Lit(math.Inf(1))), "F")
}

// A backslash in a MySQL text literal means two different strings depending
// on NO_BACKSLASH_ESCAPES, and under the default it ends the literal early:
// refused, not guessed at. A non-finite float is refused where the dialect has
// none, and spelled as float8 parses it in PostgreSQL rather than as `+Inf`,
// which PostgreSQL reads as an identifier.
func TestLiteralsRefusedWhereTheyCannotBeSpelled(t *testing.T) {
	s, err := storm.Build(&litbad{})
	if err != nil {
		t.Fatal(err)
	}
	agg := s.Table("litbads").Aggregates[0]
	if _, err := mysql.AggregateSelect("litbads", agg); err == nil || !strings.Contains(err.Error(), "backslash") {
		t.Errorf("mysql: want the backslash refused, got %v", err)
	}
	if _, err := mssql.AggregateSelect("litbads", agg); err == nil {
		t.Error("mssql: an infinite literal lowered")
	}
	pg := pgsql.AggregateSelect("litbads", agg)
	if !strings.Contains(pg, `'Infinity'::float8`) || strings.Contains(pg, "+Inf") {
		t.Errorf("postgres: %s", pg)
	}
}
