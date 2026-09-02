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
	a.Named("Everything").
		By(&m.Status).
		Count("All").
		// And / Or / Not / IsNull / IsNotNull / Lt / Lte, all unexercised.
		Count("Complex").Filter(storm.And(
		storm.Or(
			storm.Lt(&m.Amount, storm.Lit(10)),
			storm.Lte(&m.Amount, storm.Lit(20)),
		),
		storm.Not(storm.IsNull(&m.Score)),
		storm.IsNotNull(&m.Balance),
	)).
		// Coalesce / NullIf / Abs / Col, all unexercised.
		Sum(storm.Coalesce(&m.Balance, &m.Amount), "Coalesced").
		Sum(storm.NullIf(&m.Amount, storm.Lit(0)), "Nullified").
		Sum(storm.Abs(storm.Col(&m.Amount)), "Absolute")
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
