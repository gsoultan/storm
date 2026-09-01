package pgsql

import (
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Expr renders an expression tree.
//
// The only place an operator, a function name or a parenthesis is written for
// the aggregation path. Everything upstream builds a logical tree; this decides
// what PostgreSQL reads (R9, and boundaries.sh keeps the seam open).
func Expr(e schema.Expr) string {
	var b strings.Builder
	writeExpr(&b, e)
	return b.String()
}

func writeExpr(b *strings.Builder, e schema.Expr) {
	switch e.Kind {
	case schema.ExprCol:
		// Qualified only when it has to be. An unqualified column in a
		// single-table read keeps the generated SQL readable; in a join every
		// column is qualified, because "id" in a two-table query is ambiguous
		// and PostgreSQL says so.
		if e.Tbl != "" {
			b.WriteString(Ident(e.Tbl))
			b.WriteByte('.')
		}
		b.WriteString(Ident(e.Col))

	case schema.ExprStar:
		b.WriteString("*")

	case schema.ExprLit:
		b.WriteString(e.Lit.SQL())

	case schema.ExprFunc:
		b.WriteString(e.Fn)
		b.WriteByte('(')
		writeArgs(b, e.Args)
		b.WriteByte(')')

	case schema.ExprGrouping:
		b.WriteString("GROUPING(")
		writeArgs(b, e.Args)
		b.WriteByte(')')

	case schema.ExprAgg:
		b.WriteString(e.Fn)
		b.WriteByte('(')
		writeArgs(b, e.Args)
		b.WriteByte(')')
		// FILTER binds to the aggregate and must precede OVER. Written before
		// the window for that reason, not for readability:
		//   count(*) FILTER (WHERE …) OVER (…)
		if e.Filter != nil {
			b.WriteString(" FILTER (WHERE ")
			writeCond(b, *e.Filter)
			b.WriteByte(')')
		}
		if e.Over != nil {
			writeOver(b, *e.Over)
		}

	case schema.ExprWindow:
		b.WriteString(e.Fn)
		b.WriteByte('(')
		writeArgs(b, e.Args)
		b.WriteByte(')')
		if e.Over != nil {
			writeOver(b, *e.Over)
		}
	}
}

func writeArgs(b *strings.Builder, args []schema.Expr) {
	for i, a := range args {
		if i > 0 {
			b.WriteString(", ")
		}
		writeExpr(b, a)
	}
}

func writeOver(b *strings.Builder, w schema.Window) {
	b.WriteString(" OVER (")
	if len(w.PartitionBy) > 0 {
		b.WriteString("PARTITION BY ")
		writeArgs(b, w.PartitionBy)
	}
	if len(w.OrderBy) > 0 {
		if len(w.PartitionBy) > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("ORDER BY ")
		for i, t := range w.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(b, t.Expr)
			if t.Desc {
				b.WriteString(" DESC")
			}
			if t.NullsFirst != nil {
				if *t.NullsFirst {
					b.WriteString(" NULLS FIRST")
				} else {
					b.WriteString(" NULLS LAST")
				}
			}
		}
	}
	b.WriteByte(')')
}

// Cond renders a declared predicate.
func Cond(c schema.Cond) string {
	var b strings.Builder
	writeCond(&b, c)
	return b.String()
}

func writeCond(b *strings.Builder, c schema.Cond) {
	switch c.Kind {
	case schema.CondCmp:
		writeExpr(b, c.Left)
		b.WriteByte(' ')
		b.WriteString(string(c.Op))
		b.WriteByte(' ')
		writeExpr(b, c.Right)

	case schema.CondIsNull:
		writeExpr(b, c.Left)
		b.WriteString(" IS NULL")

	case schema.CondIsNotNull:
		writeExpr(b, c.Left)
		b.WriteString(" IS NOT NULL")

	case schema.CondNot:
		b.WriteString("NOT (")
		if len(c.Args) > 0 {
			writeCond(b, c.Args[0])
		}
		b.WriteByte(')')

	case schema.CondAnd, schema.CondOr:
		// Always parenthesised. Precedence between AND and OR is a classic
		// source of silently wrong predicates, and the cost of the brackets is
		// nothing — the planner does not care and a reviewer does.
		sep := " AND "
		if c.Kind == schema.CondOr {
			sep = " OR "
		}
		b.WriteByte('(')
		for i, a := range c.Args {
			if i > 0 {
				b.WriteString(sep)
			}
			writeCond(b, a)
		}
		b.WriteByte(')')
	}
}

// GroupBy renders the grouping clause, including GROUPING SETS forms.
func GroupBy(agg *schema.Aggregate) string {
	if len(agg.By) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(" GROUP BY ")
	if agg.Sets == nil {
		for i, g := range agg.By {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(&b, g.Expr)
		}
		return b.String()
	}
	switch agg.Sets.Kind {
	case schema.SetsRollup:
		b.WriteString("ROLLUP(")
		for i, g := range agg.By {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(&b, g.Expr)
		}
		b.WriteByte(')')
	case schema.SetsCube:
		b.WriteString("CUBE(")
		for i, g := range agg.By {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(&b, g.Expr)
		}
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
				writeExpr(&b, agg.By[idx].Expr)
			}
			b.WriteByte(')')
		}
		b.WriteByte(')')
	}
	return b.String()
}
