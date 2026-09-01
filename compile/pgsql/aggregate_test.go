package pgsql_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
)

func col(name string) schema.Expr { return schema.Expr{Kind: schema.ExprCol, Col: name} }

func TestExprRendering(t *testing.T) {
	cases := []struct {
		name string
		e    schema.Expr
		want string
	}{
		{"column", col("status"), `"status"`},
		{"star", schema.Expr{Kind: schema.ExprStar}, `*`},
		{"count star", schema.Expr{Kind: schema.ExprAgg, Fn: "count",
			Args: []schema.Expr{{Kind: schema.ExprStar}}}, `count(*)`},
		{"sum col", schema.Expr{Kind: schema.ExprAgg, Fn: "sum",
			Args: []schema.Expr{col("total")}}, `sum("total")`},
		{"date_trunc", schema.Expr{Kind: schema.ExprFunc, Fn: "date_trunc", Args: []schema.Expr{
			{Kind: schema.ExprLit, Lit: schema.Literal{Kind: schema.TypeText, S: "day"}},
			col("placed_at"),
		}}, `date_trunc('day', "placed_at")`},
		{"grouping", schema.Expr{Kind: schema.ExprGrouping,
			Args: []schema.Expr{col("status")}}, `GROUPING("status")`},
	}
	for _, c := range cases {
		if got := pgsql.Expr(c.e); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// FILTER binds to the aggregate and must precede OVER. Emitting them the other
// way round is a syntax error, not a style choice.
func TestFilterPrecedesOver(t *testing.T) {
	e := schema.Expr{Kind: schema.ExprAgg, Fn: "count",
		Args:   []schema.Expr{{Kind: schema.ExprStar}},
		Filter: &schema.Cond{Kind: schema.CondCmp, Op: schema.OpGt, Left: col("total"), Right: schema.Expr{Kind: schema.ExprLit, Lit: schema.Literal{Kind: schema.TypeInt8, I: 50}}},
		Over:   &schema.Window{PartitionBy: []schema.Expr{col("status")}},
	}
	want := `count(*) FILTER (WHERE "total" > 50) OVER (PARTITION BY "status")`
	if got := pgsql.Expr(e); got != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}
}

func TestWindowRendering(t *testing.T) {
	e := schema.Expr{Kind: schema.ExprWindow, Fn: "row_number", Over: &schema.Window{
		PartitionBy: []schema.Expr{col("status")},
		OrderBy: []schema.OrderTerm{
			{Expr: col("placed_at"), Desc: true},
			{Expr: col("id")},
		},
	}}
	want := `row_number() OVER (PARTITION BY "status" ORDER BY "placed_at" DESC, "id")`
	if got := pgsql.Expr(e); got != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}
}

// AND/OR precedence is a classic source of silently wrong predicates. Always
// parenthesised; the planner does not care and a reviewer does.
func TestCondAlwaysParenthesised(t *testing.T) {
	c := schema.Cond{Kind: schema.CondOr, Args: []schema.Cond{
		{Kind: schema.CondCmp, Op: schema.OpEq, Left: col("a"), Right: col("b")},
		{Kind: schema.CondAnd, Args: []schema.Cond{
			{Kind: schema.CondCmp, Op: schema.OpEq, Left: col("c"), Right: col("d")},
			{Kind: schema.CondIsNull, Left: col("e")},
		}},
	}}
	want := `("a" = "b" OR ("c" = "d" AND "e" IS NULL))`
	if got := pgsql.Cond(c); got != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}
}

// A declared string literal is escaped. There is no runtime input here, but a
// quote in a declared constant would still break the statement.
func TestLiteralEscaping(t *testing.T) {
	l := schema.Literal{Kind: schema.TypeText, S: "it's"}
	if got := l.SQL(); got != `'it''s'` {
		t.Errorf("got %s", got)
	}
}

// A decimal literal must never travel through a float on its way into the
// statement — that is the whole reason numeric exists.
func TestNumericLiteralIsNotAFloat(t *testing.T) {
	l := schema.Literal{Kind: schema.TypeNumeric, S: "0.10"}
	if got := l.SQL(); got != `'0.10'::numeric` {
		t.Errorf("got %s, want '0.10'::numeric", got)
	}
}

func groupAgg(kind schema.GroupingSetsKind, sets [][]int) *schema.Aggregate {
	return &schema.Aggregate{
		Name: "X",
		By: []schema.GroupTerm{
			{Expr: col("status"), As: "Status"},
			{Expr: col("region"), As: "Region"},
		},
		Sets:  &schema.GroupingSets{Kind: kind, Sets: sets},
		Terms: []schema.AggregateTerm{{Expr: schema.Expr{Kind: schema.ExprAgg, Fn: "count", Args: []schema.Expr{{Kind: schema.ExprStar}}}, As: "N"}},
	}
}

func TestGroupingSetForms(t *testing.T) {
	for _, c := range []struct {
		name string
		agg  *schema.Aggregate
		want string
	}{
		{"rollup", groupAgg(schema.SetsRollup, nil), `ROLLUP("status", "region")`},
		{"cube", groupAgg(schema.SetsCube, nil), `CUBE("status", "region")`},
		{"explicit", groupAgg(schema.SetsExplicit, [][]int{{0}, {1}, {}}),
			`GROUPING SETS (("status"), ("region"), ())`},
	} {
		got := pgsql.GroupBy(c.agg)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: %q missing from %s", c.name, c.want, got)
		}
	}
}

// A ROLLUP's subtotal rows sort above the detail they summarise, which is the
// order a report is read in.
func TestGroupingSetsOrderNullsFirst(t *testing.T) {
	got := pgsql.AggregateSuffix(groupAgg(schema.SetsRollup, nil))
	if !strings.Contains(got, `ORDER BY "status" NULLS FIRST, "region" NULLS FIRST`) {
		t.Errorf("got %s", got)
	}
}

// A plain GROUP BY needs no NULLS FIRST — nothing is a subtotal.
func TestPlainGroupByHasNoNullsFirst(t *testing.T) {
	a := groupAgg(schema.SetsRollup, nil)
	a.Sets = nil
	if got := pgsql.AggregateSuffix(a); strings.Contains(got, "NULLS FIRST") {
		t.Errorf("a plain GROUP BY ordered NULLS FIRST: %s", got)
	}
}

func TestHavingIsRendered(t *testing.T) {
	a := groupAgg(schema.SetsRollup, nil)
	a.Sets = nil
	a.Having = &schema.Cond{Kind: schema.CondCmp, Op: schema.OpGt,
		Left:  schema.Expr{Kind: schema.ExprAgg, Fn: "count", Args: []schema.Expr{{Kind: schema.ExprStar}}},
		Right: schema.Expr{Kind: schema.ExprLit, Lit: schema.Literal{Kind: schema.TypeInt8, I: 5}}}
	got := pgsql.AggregateSuffix(a)
	if !strings.Contains(got, "HAVING count(*) > 5") {
		t.Errorf("got %s", got)
	}
	// HAVING must sit between GROUP BY and ORDER BY.
	gi, hi, oi := strings.Index(got, "GROUP BY"), strings.Index(got, "HAVING"), strings.Index(got, "ORDER BY")
	if !(gi < hi && hi < oi) {
		t.Errorf("clauses out of order: %s", got)
	}
}
