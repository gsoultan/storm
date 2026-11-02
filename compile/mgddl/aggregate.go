package mgddl

import (
	"github.com/gsoultan/storm/compile/mongo"
	"github.com/gsoultan/storm/schema"
)

// checkAggregate holds a declared aggregation to the subset generated as a
// $group with SQL's answer: grouping by columns, and count, count of a
// column, count(DISTINCT), sum, avg, min and max over columns, ordered by
// any output. The rest of the SQL surface — grouping sets, FILTER, HAVING,
// windows, arithmetic, joins, declared WHERE and parameters, grouping by an
// expression — is refused by name.
func checkAggregate(t *schema.Table, a *schema.Aggregate, add func(pos, format string, a ...any)) {
	n := t.Name + "." + a.Name
	refuse := func(what string) {
		add(t.Pos, "%s: aggregation %s uses %s, which this back end does not generate", t.Name, a.Name, what)
	}
	switch {
	case a.Sets != nil:
		refuse("grouping sets")
	case a.Having != nil:
		refuse("HAVING")
	case len(a.Joins) > 0:
		refuse("a join")
	case a.Where != nil:
		refuse("a declared WHERE")
	case len(a.Params) > 0:
		refuse("parameters")
	}
	for _, g := range a.By {
		if g.Expr.Kind != schema.ExprCol || g.Expr.Tbl != "" {
			refuse("grouping by an expression (" + g.As + ")")
			continue
		}
		if c := t.Column(g.Expr.Col); c != nil && (c.Type.Array || mongo.IsJSON(c)) {
			refuse("grouping by an array or json column (" + g.As + ")")
		}
	}
	for _, term := range a.Terms {
		if why := AggTermRefusal(t, term.Expr); why != "" {
			add(t.Pos, "%s: %s: %s", n, term.As, why)
		}
	}
}

// AggTermRefusal is why an aggregation's output has no $group form with
// SQL's answer, or "".
func AggTermRefusal(t *schema.Table, e schema.Expr) string {
	if e.Kind != schema.ExprAgg {
		return "a window, GROUPING() or arithmetic over aggregates, which this back end does not generate"
	}
	if e.Filter != nil || e.Over != nil || len(e.Within) > 0 {
		return "FILTER, OVER or an ordered aggregate, which this back end does not generate"
	}
	if len(e.Args) != 1 {
		return "an aggregate of other than one argument"
	}
	arg := e.Args[0]
	if arg.Kind == schema.ExprStar && e.Fn == string(schema.AggCount) && !e.Distinct {
		return ""
	}
	if arg.Kind != schema.ExprCol || arg.Tbl != "" {
		return "an aggregate over an expression, which this back end does not generate"
	}
	c := t.Column(arg.Col)
	if c == nil || c.Type.Array || mongo.IsJSON(c) {
		return "an aggregate over an array or json column"
	}
	if e.Distinct && e.Fn != string(schema.AggCount) {
		return "DISTINCT other than in count"
	}
	switch schema.AggFunc(e.Fn) {
	case schema.AggCount:
		return ""
	case schema.AggSum, schema.AggAvg:
		switch c.Type.Name {
		case schema.TypeInt2, schema.TypeInt4, schema.TypeInt8, schema.TypeNumeric, schema.TypeFloat4, schema.TypeFloat8:
			return ""
		}
		return e.Fn + " of a " + c.Type.SQL() + " column"
	case schema.AggMin, schema.AggMax:
		if c.Type.Enum {
			return e.Fn + " of an enum, which PostgreSQL orders by declaration"
		}
		switch c.Type.Name {
		case schema.TypeInt2, schema.TypeInt4, schema.TypeInt8, schema.TypeNumeric, schema.TypeFloat4,
			schema.TypeFloat8, schema.TypeTimestamptz, schema.TypeDate:
			return ""
		case schema.TypeText, schema.TypeVarchar:
			return e.Fn + " of text, which PostgreSQL orders by collation and MongoDB by bytes"
		}
		return e.Fn + " of a " + c.Type.SQL() + " column"
	}
	return "the aggregate " + e.Fn + ", which this back end does not generate"
}
