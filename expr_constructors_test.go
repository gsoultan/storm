package storm_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgsql"
)

type unex struct {
	storm.Model
	Status  string
	Amount  storm.Decimal
	Score   *int32
	Balance *storm.Decimal
}

// Every scalar and condition constructor the API exports, in one declaration.
//
// Written because a v1.0 API review found ten of them — Abs, Coalesce, NullIf,
// Lt, Lte, And, Or, IsNull, IsNotNull, Col — with no call site anywhere: not in
// the example, not in a test, not internally. Their type-resolution logic had
// never executed, and the first thing running it found was that nullif could
// not express the division-by-zero guard its own documentation gives as the
// reason it exists.
func (m *unex) Aggregates(a *storm.Aggregates) {
	b := a.Named("Everything")
	b.By(&m.Status)
	b.Count("All")
	b.Count("Complex").Filter(a.And(
		a.Or(
			a.Lt(&m.Amount, a.Lit(10)),
			a.Lte(&m.Amount, a.Lit(20)),
		),
		a.Not(a.IsNull(&m.Score)),
		a.IsNotNull(&m.Balance),
	))
	b.Sum(a.Coalesce(&m.Balance, &m.Amount), "Coalesced")
	b.Sum(a.NullIf(&m.Amount, a.Lit(0)), "Nullified")
	b.Sum(a.Abs(a.Col(&m.Amount)), "Absolute")
}

func TestEveryExprConstructorLowers(t *testing.T) {
	s, err := storm.Build(&unex{})
	if err != nil {
		t.Fatalf("a declared expression does not build: %v", err)
	}
	tbl := s.Table("unexes")
	if tbl == nil || len(tbl.Aggregates) != 1 {
		t.Fatal("no aggregate")
	}
	agg := tbl.Aggregates[0]

	sql := pgsql.AggregateSelect("unexes", agg) + pgsql.AggregateSuffix(agg)
	t.Logf("lowered:\n%s", sql)

	for _, want := range []string{
		`count(*) FILTER (WHERE ((`, // And of an Or
		` OR `, ` AND `, `NOT (`,
		`IS NULL`, `IS NOT NULL`,
		`coalesce(`, `nullif(`, `abs(`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing from the lowered SQL: %q", want)
		}
	}

	// nullif is ALWAYS nullable — that is its whole job as a division guard.
	// coalesce is non-null when any argument is.
	for _, c := range agg.Terms {
		switch c.As {
		case "Nullified":
			if !c.Expr.Nullable {
				t.Error("sum(nullif(...)) is not nullable; nullif always is")
			}
		case "Absolute":
			if c.Expr.Type.Name == "" {
				t.Error("abs() resolved to no type")
			}
		}
	}
}
