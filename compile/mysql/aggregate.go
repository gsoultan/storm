package mysql

import (
	"errors"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Declared grouped reads.
//
// The plain GROUP BY / HAVING / ORDER BY shape carries over. Two things do not,
// and neither is a spelling:
//
//   - GROUPING SETS and CUBE do not exist. MySQL has only WITH ROLLUP, which is
//     strictly weaker: it produces the subtotals along ONE prefix of the
//     grouping columns, where GROUPING SETS names arbitrary combinations.
//     Measured: Error 1064. Refused, because emitting a ROLLUP for a CUBE would
//     return FEWER rows than the declaration asked for, silently.
//   - ROLLUP is a SUFFIX, not a function. `GROUP BY a, b WITH ROLLUP` rather
//     than `GROUP BY ROLLUP(a, b)`.
//
// A third is handled rather than refused: PostgreSQL orders a rollup's output
// NULLS FIRST, so subtotal rows sit above the detail they summarise. MySQL has
// no such placement — but it already sorts NULLs first ASCENDING, which is the
// same thing for the common case. Only a DESCENDING key needs help, and there
// `ISNULL(x) DESC` buys it at the cost of a sort.
//
// FILTER (WHERE …) is refused by the expression renderer, which is where an
// aggregate's measures are rendered.

// ErrNoGroupingSets is why an aggregation over arbitrary grouping combinations
// cannot be lowered.
var ErrNoGroupingSets = errors.New(
	"compile/mysql: MySQL has no GROUPING SETS or CUBE — only WITH ROLLUP, which produces " +
		"subtotals along one prefix of the grouping columns rather than arbitrary " +
		"combinations. Emitting a rollup instead would return fewer rows than the " +
		"declaration asked for, so it is refused")

// AggregateSelect is everything before the WHERE clause of a grouped read.
func AggregateSelect(table string, agg *schema.Aggregate) (string, error) {
	var b strings.Builder
	var w exprWriter
	for i, g := range agg.By {
		if i > 0 {
			b.WriteString(", ")
		}
		w.b.Reset()
		w.writeExpr(g.Expr)
		b.WriteString(w.b.String())
		b.WriteString(" AS ")
		b.WriteString(Ident(columnCase(g.As)))
	}
	for i, t := range agg.Terms {
		if i > 0 || len(agg.By) > 0 {
			b.WriteString(", ")
		}
		w.b.Reset()
		w.writeExpr(t.Expr)
		b.WriteString(w.b.String())
		b.WriteString(" AS ")
		b.WriteString(Ident(columnCase(t.As)))
	}
	return "SELECT " + b.String() + " FROM " + Ident(table), w.err
}

// GroupBy renders the grouping clause.
func GroupBy(agg *schema.Aggregate) (string, error) {
	if len(agg.By) == 0 {
		return "", nil
	}
	if agg.Sets != nil && agg.Sets.Kind != schema.SetsRollup {
		return "", ErrNoGroupingSets
	}
	var b strings.Builder
	var w exprWriter
	b.WriteString(" GROUP BY ")
	for i, g := range agg.By {
		if i > 0 {
			b.WriteString(", ")
		}
		w.b.Reset()
		w.writeExpr(g.Expr)
		b.WriteString(w.b.String())
	}
	if agg.Sets != nil {
		// A suffix here, not a function wrapping the list.
		b.WriteString(" WITH ROLLUP")
	}
	return b.String(), w.err
}

// AggregateSuffix is the GROUP BY, HAVING and ORDER BY that follow the
// predicates, in the order SQL wants them.
//
// The ordering is not cosmetic: MySQL promises no order for a GROUP BY either,
// and an unordered result makes a paginated report shuffle between requests.
func AggregateSuffix(agg *schema.Aggregate) (string, error) {
	var b strings.Builder
	var w exprWriter

	g, err := GroupBy(agg)
	if err != nil {
		return "", err
	}
	b.WriteString(g)

	if agg.Having != nil {
		b.WriteString(" HAVING ")
		w.b.Reset()
		w.writeCond(*agg.Having)
		b.WriteString(w.b.String())
	}

	// subtotalsFirst writes an ordering key that keeps a rollup's subtotal rows
	// above the detail they summarise.
	//
	// Ascending needs nothing: MySQL already sorts NULLs first, which is where
	// a subtotal belongs. Descending does — and pays a sort for it, because
	// ISNULL() is an expression and no index on the column satisfies it.
	subtotalsFirst := func(ident string, desc bool) {
		if agg.Sets == nil || !desc {
			return
		}
		b.WriteString("ISNULL(")
		b.WriteString(ident)
		b.WriteString(") DESC, ")
	}

	if len(agg.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, o := range agg.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			id := Ident(columnCase(o.As))
			if isGrouping(agg, o.As) {
				subtotalsFirst(id, o.Desc)
			}
			b.WriteString(id)
			if o.Desc {
				b.WriteString(" DESC")
			}
		}
		// Every grouping column not already named is appended as a tiebreak: a
		// measure is not unique, and a top-N report is exactly the query that
		// pages.
		for _, gb := range agg.By {
			if named(agg.OrderBy, gb.As) {
				continue
			}
			b.WriteString(", ")
			b.WriteString(Ident(columnCase(gb.As)))
		}
		return b.String(), w.err
	}

	if len(agg.By) > 0 {
		b.WriteString(" ORDER BY ")
		for i, gb := range agg.By {
			if i > 0 {
				b.WriteString(", ")
			}
			// By the ALIAS, not by repeating the expression: for a rollup the
			// expression is NULL in subtotal rows and the alias is what the
			// outer scope sees.
			b.WriteString(Ident(columnCase(gb.As)))
		}
	}
	return b.String(), w.err
}

// named reports whether an ordering already mentions an output.
func named(order []schema.AggOrder, as string) bool {
	for _, o := range order {
		if o.As == as {
			return true
		}
	}
	return false
}

// isGrouping reports whether an output is one of the grouping columns — the
// only ones a rollup makes NULL.
func isGrouping(agg *schema.Aggregate, as string) bool {
	for _, g := range agg.By {
		if g.As == as {
			return true
		}
	}
	return false
}
