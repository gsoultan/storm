package pgsql

import (
	"strings"

	"github.com/gsoultan/storm/schema"
)

// AggregateSelect is everything before the WHERE clause of a grouped read.
//
// Split at WHERE for the same reason every other read is: the call-site
// predicates are dynamic and spliced in by the runtime, while this prefix is
// fixed at generation time and never rebuilt.
func AggregateSelect(table string, agg *schema.Aggregate) string {
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, g := range agg.By {
		if i > 0 {
			b.WriteString(", ")
		}
		writeExpr(&b, g.Expr)
		b.WriteString(" AS ")
		b.WriteString(Ident(ColumnCase(g.As)))
	}
	for i, t := range agg.Terms {
		if i > 0 || len(agg.By) > 0 {
			b.WriteString(", ")
		}
		writeExpr(&b, t.Expr)
		// Aliased so the result column order is not the only thing tying a
		// value to a field, and so EXPLAIN output is readable.
		b.WriteString(" AS ")
		b.WriteString(Ident(ColumnCase(t.As)))
	}
	b.WriteString(" FROM ")
	b.WriteString(Ident(table))
	return b.String()
}

// AggregateSuffix is the GROUP BY, HAVING and ORDER BY that follow the
// predicates, in the order SQL wants them.
//
// The ordering is not cosmetic. PostgreSQL does not promise an order for a
// GROUP BY, and an unordered result makes a paginated report shuffle between
// requests and a golden test flap. The grouping expressions are a total order
// over the output by construction, so ordering by them costs nothing the
// grouping did not already pay for.
func AggregateSuffix(agg *schema.Aggregate) string {
	var b strings.Builder
	b.WriteString(GroupBy(agg))

	if agg.Having != nil {
		b.WriteString(" HAVING ")
		writeCond(&b, *agg.Having)
	}

	// An explicit ordering replaces the default entirely. It may name a
	// measure, which the default cannot: the default exists to make a grouped
	// read deterministic, and "by revenue, descending" is a different job.
	if len(agg.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, o := range agg.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(Ident(ColumnCase(o.As)))
			if o.Desc {
				b.WriteString(" DESC")
			}
			// Same NULLS FIRST rule as the default, and only for a grouping
			// column: a subtotal row's NULL is the summary of the detail below
			// it, not a missing value to be swept to one end.
			if agg.Sets != nil && isGrouping(agg, o.As) {
				b.WriteString(" NULLS FIRST")
			}
		}
		// Every grouping column not already named is appended as a tiebreak.
		//
		// A measure is not unique. `ORDER BY revenue DESC LIMIT 10 OFFSET 10`
		// over groups that tie is free to return a row on both pages and
		// another on neither — PostgreSQL makes no promise about the order of
		// equal keys, and a top-N report is exactly the query that pages. The
		// grouping columns ARE unique per row of a grouped read, so appending
		// them costs nothing at the top of the sort and makes paging total.
		for _, g := range agg.By {
			if named(agg.OrderBy, g.As) {
				continue
			}
			b.WriteString(", ")
			b.WriteString(Ident(ColumnCase(g.As)))
			if agg.Sets != nil {
				b.WriteString(" NULLS FIRST")
			}
		}
		return b.String()
	}

	if len(agg.By) > 0 {
		b.WriteString(" ORDER BY ")
		for i, g := range agg.By {
			if i > 0 {
				b.WriteString(", ")
			}
			// Ordered by the ALIAS, not by repeating the expression. For a
			// grouping set the expression is NULL in subtotal rows and the
			// alias is what the outer scope sees; repeating a date_trunc call
			// would also invite the planner to compute it twice.
			b.WriteString(Ident(ColumnCase(g.As)))
			// NULLS FIRST puts a ROLLUP's subtotal rows above the detail they
			// summarise, which is the order a report is read in.
			if agg.Sets != nil {
				b.WriteString(" NULLS FIRST")
			}
		}
	}
	return b.String()
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

// isGrouping reports whether an output name is one of the grouping columns
// rather than a measure.
func isGrouping(agg *schema.Aggregate, as string) bool {
	for _, g := range agg.By {
		if g.As == as {
			return true
		}
	}
	return false
}

// ColumnCase turns an exported Go identifier into snake_case, matching the
// convention the rest of the schema uses for column names. Exported because
// codegen derives the same alias for its synthetic columns, and two
// implementations of one rule is how they drift apart.
func ColumnCase(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			// A boundary is a lower→upper transition, or an upper followed by
			// a lower — so "OrderID" is order_id and not order_i_d.
			if i > 0 && (isLower(s[i-1]) || (i+1 < len(s) && isLower(s[i+1]))) {
				b.WriteByte('_')
			}
			b.WriteByte(c - 'A' + 'a')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
