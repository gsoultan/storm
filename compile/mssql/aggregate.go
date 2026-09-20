package mssql

import (
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Declared grouped reads.
//
// This is the construct where SQL Server is closest to PostgreSQL and furthest
// from MySQL. GROUPING SETS, CUBE and ROLLUP all exist, in the FUNCTION form —
// `GROUP BY ROLLUP(a, b)` rather than MySQL's `GROUP BY a, b WITH ROLLUP` — so
// an aggregation over arbitrary grouping combinations is emitted here and
// refused there. So is GROUPING(), which is what tells a subtotal row from a
// detail row carrying a NULL.
//
// One thing still needs help. PostgreSQL orders a rollup's output NULLS FIRST,
// so subtotal rows sit above the detail they summarise. SQL Server, like MySQL,
// sorts NULLs first ASCENDING already — so only a DESCENDING key needs a
// leading sort term, and here that term is a CASE rather than MySQL's ISNULL().
//
// FILTER (WHERE …) is refused by the expression renderer, which is where an
// aggregate's measures are rendered.

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
//
// The grouping-set forms wrap the column list rather than following it, which
// is the standard spelling and PostgreSQL's — MySQL's trailing WITH ROLLUP is
// the odd one out.
func GroupBy(agg *schema.Aggregate) (string, error) {
	if len(agg.By) == 0 {
		return "", nil
	}
	var b strings.Builder
	var w exprWriter
	writeAll := func() {
		for i, g := range agg.By {
			if i > 0 {
				b.WriteString(", ")
			}
			w.b.Reset()
			w.writeExpr(g.Expr)
			b.WriteString(w.b.String())
		}
	}
	b.WriteString(" GROUP BY ")
	if agg.Sets == nil {
		writeAll()
		return b.String(), w.err
	}
	switch agg.Sets.Kind {
	case schema.SetsRollup:
		b.WriteString("ROLLUP(")
		writeAll()
		b.WriteByte(')')
	case schema.SetsCube:
		b.WriteString("CUBE(")
		writeAll()
		b.WriteByte(')')
	default:
		b.WriteString("GROUPING SETS (")
		for i, set := range agg.Sets.Sets {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('(')
			for j, idx := range set {
				if j > 0 {
					b.WriteString(", ")
				}
				w.b.Reset()
				w.writeExpr(agg.By[idx].Expr)
				b.WriteString(w.b.String())
			}
			b.WriteByte(')')
		}
		b.WriteByte(')')
	}
	return b.String(), w.err
}

// AggregateSuffix is the GROUP BY, HAVING and ORDER BY that follow the
// predicates, in the order SQL wants them.
//
// The ordering is not cosmetic: SQL Server promises no order for a GROUP BY
// either, and an unordered result makes a paginated report shuffle between
// requests.
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
	// Ascending needs nothing: SQL Server already sorts NULLs first, which is
	// where a subtotal belongs. Descending does — and pays a sort for it,
	// because the CASE is an expression and no index on the column satisfies
	// it. MySQL's ISNULL() does not exist here; the CASE is the portable form.
	subtotalsFirst := func(ident string, desc bool) {
		if agg.Sets == nil || !desc {
			return
		}
		b.WriteString("CASE WHEN ")
		b.WriteString(ident)
		b.WriteString(" IS NULL THEN 0 ELSE 1 END, ")
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
