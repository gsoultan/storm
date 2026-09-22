package oracle

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Expression rendering for Oracle.
//
// Two refusals, one fewer than SQL Server, and one gain neither MySQL nor SQL
// Server has.
//
//   - `FILTER (WHERE …)` on an aggregate does not exist here either. The
//     rewrite is `SUM(CASE WHEN … THEN x END)`, which is NOT a spelling: it
//     changes what `count(*)` counts, and getting it wrong is a wrong number
//     with no error. Refused for the same reason the other two are.
//   - A numeric RANGE frame bound. Oracle has window frames, and `RANGE
//     BETWEEN 3 PRECEDING` requires the ORDER BY key to be a single numeric or
//     date column it can do arithmetic on — so it is legal for some orderings
//     and ORA-30486 for others. storm refuses it rather than emit a frame that
//     depends on a column type the declaration does not pin.
//
// THE GAIN: NULLS FIRST / NULLS LAST work inside OVER(). SQL Server has them
// nowhere and refuses a window ordering that names one, because dropping the
// placement would change which rows the frame covers. Oracle has them
// everywhere an ORDER BY appears, so nothing is refused and nothing is
// silently dropped.
//
// AND ONE DIVISION DIFFERENCE. Oracle's `/` on two integers yields a NUMBER
// with a fraction — it does not truncate the way SQL Server's INT division
// does — so the CAST that back end needs is not needed here. The ROUND to the
// declared scale stays, because the declaration said what scale it wanted.

// ErrNoFilterClause is why an aggregate's FILTER cannot be lowered.
//
// Oracle has no `FILTER (WHERE …)`. The rewrite everybody reaches for is
// `SUM(CASE WHEN … THEN x END)`, and it is NOT a spelling: it changes what
// `count(*)` counts, because a CASE that yields NULL is still a row. Getting it
// wrong is a wrong number with no error, which is why storm refuses rather than
// rewrites — the same cut compile/mysql and compile/mssql make.
var ErrNoFilterClause = errors.New(
	"compile/oracle: Oracle has no FILTER (WHERE ...) on an aggregate, and rewriting it " +
		"as SUM(CASE WHEN ... END) changes what count(*) counts rather than how it is " +
		"spelled\n      write the CASE yourself if that is what you mean")

// ErrNoRangeOffset is why a numeric RANGE frame bound cannot be lowered.
//
// Oracle accepts `RANGE BETWEEN 3 PRECEDING` only when the ORDER BY key is a
// single column it can do arithmetic on, and raises ORA-30486 otherwise. That
// makes the frame's validity depend on a column type the declaration does not
// pin, so it is refused rather than emitted and hoped for. ROWS accepts the
// offset unconditionally and means something different over ties, so it is not
// a substitution storm may make.
var ErrNoRangeOffset = errors.New(
	"compile/oracle: a numeric RANGE frame bound is valid here only for an ordering " +
		"Oracle can do arithmetic on, and ORA-30486 otherwise\n" +
		"      use ROWS if you mean a row count — it is not the same frame over ties")

type exprWriter struct {
	b   strings.Builder
	err error
}

func (w *exprWriter) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}

// Expr renders one expression.
func Expr(e schema.Expr) (string, error) {
	var w exprWriter
	w.writeExpr(e)
	return w.b.String(), w.err
}

// Cond renders one condition.
func Cond(c schema.Cond) (string, error) {
	var w exprWriter
	w.writeCond(c)
	return w.b.String(), w.err
}

func (w *exprWriter) writeExpr(e schema.Expr) {
	switch e.Kind {
	case schema.ExprCol:
		if e.Tbl != "" {
			w.b.WriteString(Ident(e.Tbl))
			w.b.WriteByte('.')
		}
		w.b.WriteString(Ident(e.Col))

	case schema.ExprStar:
		w.b.WriteString("*")

	case schema.ExprLit:
		w.b.WriteString(e.Lit.SQL())

	case schema.ExprParam:
		// Numbered at generate time, not counted at splice time: a union has no
		// token stream to number against, and the same parameter used in two
		// branches must render as the SAME name or the caller would have to
		// pass it twice. `:1` is a NAME here that happens to look like an
		// ordinal, so a second use of it binds the same value — the property
		// MySQL lacks and has to refuse a reused parameter over.
		w.b.WriteString(Placeholder)
		w.b.WriteString(strconv.Itoa(e.Param + 1))

	case schema.ExprFunc:
		w.b.WriteString(e.Fn)
		w.b.WriteByte('(')
		w.writeArgs(e.Args)
		w.b.WriteByte(')')

	case schema.ExprBinary:
		w.writeBinary(e)

	case schema.ExprGrouping:
		// GROUPING() is real here — Oracle has GROUPING SETS, CUBE and ROLLUP
		// in full, measured in internal/oraclespike — so unlike MySQL there is
		// nothing to refuse alongside it. See aggregate.go.
		w.b.WriteString("GROUPING(")
		w.writeArgs(e.Args)
		w.b.WriteByte(')')

	case schema.ExprAgg:
		w.b.WriteString(e.Fn)
		w.b.WriteByte('(')
		if e.Distinct {
			w.b.WriteString("DISTINCT ")
		}
		w.writeArgs(e.Args)
		w.b.WriteByte(')')
		if e.Filter != nil {
			w.fail(ErrNoFilterClause)
		}
		if e.Over != nil {
			w.writeOver(*e.Over)
		}

	case schema.ExprWindow:
		w.b.WriteString(e.Fn)
		w.b.WriteByte('(')
		w.writeArgs(e.Args)
		w.b.WriteByte(')')
		if e.Over != nil {
			w.writeOver(*e.Over)
		}
	}
}

// writeBinary renders arithmetic, always parenthesised — the IR is a tree, so
// the grouping is already decided and precedence must not re-decide it.
func (w *exprWriter) writeBinary(e schema.Expr) {
	if len(e.Args) != 2 {
		return
	}
	if e.Arith == schema.ArithDiv {
		// NO CAST, unlike SQL Server. Oracle's NUMBER arithmetic keeps the
		// fraction: `total / count` on two NUMBER(19) columns is a NUMBER with
		// a scale, where SQL Server's INT division truncates and needs the
		// left operand widened first. Only the ROUND is needed, because the
		// declaration said what scale it wanted.
		scale := e.Type.Scale
		w.b.WriteString("ROUND(")
		w.writeExpr(e.Args[0])
		w.writeArith(e.Arith)
		w.writeExpr(e.Args[1])
		w.b.WriteString(", ")
		w.b.WriteString(strconv.Itoa(scale))
		w.b.WriteByte(')')
		return
	}
	w.b.WriteByte('(')
	w.writeExpr(e.Args[0])
	w.writeArith(e.Arith)
	w.writeExpr(e.Args[1])
	w.b.WriteByte(')')
}

// writeArith spells the operator, and refuses one it does not know rather than
// falling through to a token that means something else here.
func (w *exprWriter) writeArith(op schema.ArithOp) {
	switch op {
	case schema.ArithAdd:
		w.b.WriteString(" + ")
	case schema.ArithSub:
		w.b.WriteString(" - ")
	case schema.ArithMul:
		w.b.WriteString(" * ")
	case schema.ArithDiv:
		w.b.WriteString(" / ")
	default:
		w.fail(fmt.Errorf("compile/oracle: no lowering for arithmetic operator %v", op))
	}
}

func (w *exprWriter) writeArgs(args []schema.Expr) {
	for i, a := range args {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.writeExpr(a)
	}
}

func (w *exprWriter) writeCond(c schema.Cond) {
	switch c.Kind {
	case schema.CondCmp:
		w.writeExpr(c.Left)
		w.b.WriteByte(' ')
		w.b.WriteString(string(c.Op))
		w.b.WriteByte(' ')
		w.writeExpr(c.Right)

	case schema.CondIsNull:
		w.writeExpr(c.Left)
		w.b.WriteString(" IS NULL")

	case schema.CondIsNotNull:
		w.writeExpr(c.Left)
		w.b.WriteString(" IS NOT NULL")

	case schema.CondNot:
		w.b.WriteString("NOT (")
		if len(c.Args) > 0 {
			w.writeCond(c.Args[0])
		}
		w.b.WriteByte(')')

	case schema.CondAnd, schema.CondOr:
		// Always parenthesised, for the reason PostgreSQL's is: AND/OR
		// precedence is a classic source of silently wrong predicates and the
		// brackets cost nothing.
		sep := " AND "
		if c.Kind == schema.CondOr {
			sep = " OR "
		}
		w.b.WriteByte('(')
		for i, a := range c.Args {
			if i > 0 {
				w.b.WriteString(sep)
			}
			w.writeCond(a)
		}
		w.b.WriteByte(')')
	}
}

func (w *exprWriter) writeOver(win schema.Window) {
	w.b.WriteString(" OVER (")
	if len(win.PartitionBy) > 0 {
		w.b.WriteString("PARTITION BY ")
		w.writeArgs(win.PartitionBy)
	}
	if len(win.OrderBy) > 0 {
		if len(win.PartitionBy) > 0 {
			w.b.WriteByte(' ')
		}
		w.b.WriteString("ORDER BY ")
		for i, t := range win.OrderBy {
			if i > 0 {
				w.b.WriteString(", ")
			}
			w.writeExpr(t.Expr)
			if t.Desc {
				w.b.WriteString(" DESC")
			}
			// SPELLED, not refused. Oracle has NULLS FIRST/LAST everywhere an
			// ORDER BY appears, inside OVER() included — which SQL Server has
			// nowhere, and which it therefore has to refuse because dropping
			// the placement would change which rows the frame covers.
			if t.NullsFirst != nil {
				if *t.NullsFirst {
					w.b.WriteString(" NULLS FIRST")
				} else {
					w.b.WriteString(" NULLS LAST")
				}
			}
		}
	}
	if win.Frame != nil {
		w.writeFrame(*win.Frame)
	}
	w.b.WriteByte(')')
}

// writeFrame renders ROWS/RANGE BETWEEN.
//
// Always BETWEEN, never the one-bound shorthand, for the reason PostgreSQL's
// is: `ROWS 3 PRECEDING` means `BETWEEN 3 PRECEDING AND CURRENT ROW`, which is
// a rule people misremember in both directions.
func (w *exprWriter) writeFrame(f schema.Frame) {
	if f.Kind == schema.FrameRange {
		// Oracle's numeric RANGE bound is legal only when the ORDER BY key is
		// a single column it can do arithmetic on, and ORA-30486 otherwise —
		// so it is a frame whose validity depends on a column type the
		// declaration does not pin. Refused rather than emitted conditionally.
		if offsetBound(f.Start) || offsetBound(f.End) {
			w.fail(ErrNoRangeOffset)
		}
		w.b.WriteString(" RANGE BETWEEN ")
	} else {
		w.b.WriteString(" ROWS BETWEEN ")
	}
	w.writeBound(f.Start)
	w.b.WriteString(" AND ")
	w.writeBound(f.End)
}

// offsetBound reports whether a bound names a row count, which RANGE cannot.
func offsetBound(b schema.FrameBound) bool {
	return b.Kind == schema.Preceding || b.Kind == schema.Following
}

func (w *exprWriter) writeBound(bound schema.FrameBound) {
	switch bound.Kind {
	case schema.UnboundedPreceding:
		w.b.WriteString("UNBOUNDED PRECEDING")
	case schema.Preceding:
		w.b.WriteString(strconv.Itoa(bound.N))
		w.b.WriteString(" PRECEDING")
	case schema.CurrentRow:
		w.b.WriteString("CURRENT ROW")
	case schema.Following:
		w.b.WriteString(strconv.Itoa(bound.N))
		w.b.WriteString(" FOLLOWING")
	case schema.UnboundedFollowing:
		w.b.WriteString("UNBOUNDED FOLLOWING")
	}
}
