package pgsql

import (
	"strconv"
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

	case schema.ExprBinary:
		writeBinary(b, e)

	case schema.ExprGrouping:
		b.WriteString("GROUPING(")
		writeArgs(b, e.Args)
		b.WriteByte(')')

	case schema.ExprAgg:
		// avg() over an integer or a numeric divides, and PostgreSQL's numeric
		// division chooses a scale large enough to overflow the eighteen
		// significant digits a Decimal holds. The result type carries the
		// scale storm declared; this makes the statement agree with it. Sum
		// needs no such thing — its scale is the input's.
		round := e.Fn == "avg" && e.Type.Name == schema.TypeNumeric && e.Type.Scale > 0
		if round {
			b.WriteString("round(")
		}
		b.WriteString(e.Fn)
		b.WriteByte('(')
		if e.Distinct {
			b.WriteString("DISTINCT ")
		}
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
		if round {
			// After the window, not before: round() takes the value the window
			// produced, and rounding inside would average the rounded groups.
			b.WriteString(", ")
			b.WriteString(strconv.Itoa(e.Type.Scale))
			b.WriteByte(')')
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

// writeBinary renders arithmetic, always parenthesised.
//
// Parenthesised unconditionally rather than by precedence: the IR is a tree, so
// the grouping is already decided, and emitting `a + b * c` would make the SQL
// mean something the tree does not say. Nobody reads generated SQL for its
// spacing.
//
// The cast is the part that matters. PostgreSQL's `/` on two integers
// truncates, so `count(paid) / count(*)` is 0 for every group that is not
// entirely paid. schema.BinaryResult already types that expression as numeric;
// this makes the SQL agree. It lives HERE, not in the front end, because it is
// a PostgreSQL fact — MySQL's `/` already yields a decimal, and its back end
// must not inherit a cast it does not need.
func writeBinary(b *strings.Builder, e schema.Expr) {
	if len(e.Args) != 2 {
		return
	}
	if e.Arith == schema.ArithDiv {
		// round(), because PostgreSQL's numeric division chooses its own scale
		// and it is a large one: 1/4 is 0.25000000000000000000, which is more
		// significant digits than a Decimal holds. The scale came from the
		// declaration, so the statement says it too.
		b.WriteString("round(")
		writeExpr(b, e.Args[0])
		if e.Args[0].Type.Name != schema.TypeNumeric {
			b.WriteString("::numeric")
		}
		b.WriteString(arithOp(e.Arith))
		writeExpr(b, e.Args[1])
		b.WriteString(", ")
		b.WriteString(strconv.Itoa(e.Type.Scale))
		b.WriteByte(')')
		return
	}
	b.WriteByte('(')
	writeExpr(b, e.Args[0])
	b.WriteString(arithOp(e.Arith))
	writeExpr(b, e.Args[1])
	b.WriteByte(')')
}

// arithOp spells the operator. The only place in storm that does.
func arithOp(op schema.ArithOp) string {
	switch op {
	case schema.ArithAdd:
		return " + "
	case schema.ArithSub:
		return " - "
	case schema.ArithMul:
		return " * "
	case schema.ArithDiv:
		return " / "
	}
	return " ? "
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
	if w.Frame != nil {
		writeFrame(b, *w.Frame)
	}
	b.WriteByte(')')
}

// writeFrame renders ROWS/RANGE BETWEEN.
//
// Always BETWEEN, never the one-bound shorthand. `ROWS 3 PRECEDING` means
// `BETWEEN 3 PRECEDING AND CURRENT ROW`, which is a rule people misremember in
// both directions; spelling both edges makes the generated SQL say what the
// declaration said.
func writeFrame(b *strings.Builder, f schema.Frame) {
	if f.Kind == schema.FrameRange {
		b.WriteString(" RANGE BETWEEN ")
	} else {
		b.WriteString(" ROWS BETWEEN ")
	}
	writeBound(b, f.Start)
	b.WriteString(" AND ")
	writeBound(b, f.End)
}

func writeBound(b *strings.Builder, bound schema.FrameBound) {
	switch bound.Kind {
	case schema.UnboundedPreceding:
		b.WriteString("UNBOUNDED PRECEDING")
	case schema.Preceding:
		b.WriteString(strconv.Itoa(bound.N))
		b.WriteString(" PRECEDING")
	case schema.CurrentRow:
		b.WriteString("CURRENT ROW")
	case schema.Following:
		b.WriteString(strconv.Itoa(bound.N))
		b.WriteString(" FOLLOWING")
	case schema.UnboundedFollowing:
		b.WriteString("UNBOUNDED FOLLOWING")
	}
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
