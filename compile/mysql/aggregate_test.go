package mysql_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/mysql"
	"github.com/gsoultan/storm/schema"
)

func regionAgg(sets *schema.GroupingSets) *schema.Aggregate {
	return &schema.Aggregate{
		Name: "ByRegion",
		By: []schema.GroupTerm{
			{Expr: schema.Expr{Kind: schema.ExprCol, Col: "region"}, As: "Region"},
		},
		Terms: []schema.AggregateTerm{
			{Expr: schema.Expr{Kind: schema.ExprAgg, Fn: "sum",
				Args: []schema.Expr{{Kind: schema.ExprCol, Col: "amt"}}}, As: "Total"},
		},
		Sets: sets,
	}
}

// ROLLUP is a SUFFIX in MySQL, not a function wrapping the grouping list.
func TestRollupIsASuffix(t *testing.T) {
	got, err := mysql.GroupBy(regionAgg(&schema.GroupingSets{Kind: schema.SetsRollup}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, " WITH ROLLUP") {
		t.Errorf("rollup is not a suffix: %s", got)
	}
	if strings.Contains(got, "ROLLUP(") {
		t.Errorf("the PostgreSQL function form reached MySQL SQL: %s", got)
	}
}

// GROUPING SETS and CUBE do not exist — measured, Error 1064. Emitting a rollup
// for a cube would return FEWER rows than the declaration asked for, silently,
// which is why this refuses instead of approximating.
func TestGroupingSetsAndCubeAreRefused(t *testing.T) {
	for _, k := range []schema.GroupingSetsKind{schema.SetsCube, schema.SetsExplicit} {
		_, err := mysql.GroupBy(regionAgg(&schema.GroupingSets{Kind: k}))
		if !errors.Is(err, mysql.ErrNoGroupingSets) {
			t.Errorf("kind %v returned %v; it must refuse", k, err)
		}
	}
	// A plain grouping, and a rollup, are both fine.
	if _, err := mysql.GroupBy(regionAgg(nil)); err != nil {
		t.Errorf("a plain GROUP BY was refused: %v", err)
	}
}

// PostgreSQL orders a rollup NULLS FIRST so subtotals sit above their detail.
// MySQL has no placement — but already sorts NULLs first ASCENDING, which is
// the same thing. Only a DESCENDING key needs help.
func TestSubtotalsStayAboveTheirDetail(t *testing.T) {
	agg := regionAgg(&schema.GroupingSets{Kind: schema.SetsRollup})

	asc, err := mysql.AggregateSuffix(agg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(asc, "ISNULL") {
		t.Errorf("an ascending key bought a sort it did not need: %s", asc)
	}
	if strings.Contains(strings.ToUpper(asc), "NULLS") {
		t.Errorf("a NULLS placement MySQL rejects reached the SQL: %s", asc)
	}

	agg.OrderBy = []schema.AggOrder{{As: "Region", Desc: true}}
	desc, err := mysql.AggregateSuffix(agg)
	if err != nil {
		t.Fatal(err)
	}
	// Descending puts NULLs last on MySQL, which would bury the subtotals
	// under the detail they summarise.
	if !strings.Contains(desc, "ISNULL(`region`) DESC") {
		t.Errorf("a descending rollup key does not keep subtotals first: %s", desc)
	}
}

// A measure is not unique, and a top-N report is exactly the query that pages,
// so the grouping columns are appended as a tiebreak.
func TestOrderingByAMeasureStillTiebreaks(t *testing.T) {
	agg := regionAgg(nil)
	agg.OrderBy = []schema.AggOrder{{As: "Total", Desc: true}}
	got, err := mysql.AggregateSuffix(agg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "`total` DESC, `region`") {
		t.Errorf("the grouping column was not appended as a tiebreak: %s", got)
	}
}

// FILTER is refused by the expression renderer, which is where a measure is
// rendered — so an aggregate carrying one cannot be emitted either.
func TestAggregateWithAFilterIsRefused(t *testing.T) {
	agg := regionAgg(nil)
	cond := schema.Cond{Kind: schema.CondIsNotNull,
		Left: schema.Expr{Kind: schema.ExprCol, Col: "amt"}}
	agg.Terms[0].Expr.Filter = &cond
	_, err := mysql.AggregateSelect("sales", agg)
	if !errors.Is(err, mysql.ErrNoFilterClause) {
		t.Fatalf("an aggregate with a FILTER returned %v; MySQL has no such clause", err)
	}
}
