package mssql

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Expression rendering for SQL Server.
//
// Where compile/mysql had to refuse five things, this refuses two — and gains
// one MySQL could not have:
//
//   - `FILTER (WHERE …)` on an aggregate does not exist here either. The
//     rewrite is `SUM(CASE WHEN … THEN x END)`, which is NOT a spelling: it
//     changes what `count(*)` counts, and getting it wrong is a wrong number
//     with no error. Refused for the same reason MySQL's is.
//   - A numeric RANGE frame bound. SQL Server has window frames, but RANGE
//     takes only UNBOUNDED and CURRENT ROW — `RANGE BETWEEN 3 PRECEDING` is an
//     error, where ROWS accepts it. Refused with the fix named, because ROWS
//     and RANGE mean different things over ties and swapping one for the other
//     would change the frame.
//
// And the gain: a DECLARED PARAMETER MAY BE REUSED. Parameters are named here,
// so `@p1` in two union branches binds one value, exactly as PostgreSQL's `$1`
// does. compile/mysql has to refuse that outright — position is what binds a
// value there, so a second use is a second binding and a different arity than
// the caller was generated against.
//
// Errors accumulate on the writer rather than being returned from every call,
// because these render into a strings.Builder deep inside a tree walk and a
// returned error at each node would be noise at fifty call sites.

// ErrNoFilterClause is why an aggregate with a FILTER cannot be lowered.
var ErrNoFilterClause = errors.New(
	"compile/mssql: SQL Server has no FILTER (WHERE …) clause on an aggregate. The rewrite is " +
		"SUM(CASE WHEN … THEN … END), which changes what the aggregate counts rather than " +
		"how it is spelled, so storm will not do it silently")

// ErrNoRangeOffset is why a numeric RANGE frame cannot be lowered.
var ErrNoRangeOffset = errors.New(
	"compile/mssql: SQL Server's RANGE frame accepts only UNBOUNDED and CURRENT ROW, not a " +
		"row offset. ROWS accepts the offset, but ROWS and RANGE differ over ties — RANGE " +
		"includes every peer of the boundary row — so substituting one changes the frame " +
		"the aggregate reads")

// exprWriter renders expressions, carrying the first thing it could not lower.
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
		// pass it twice.
		w.b.WriteString(Placeholder)
		w.b.WriteString("p")
		w.b.WriteString(strconv.Itoa(e.Param + 1))

	case schema.ExprFunc:
		w.b.WriteString(e.Fn)
		w.b.WriteByte('(')
		w.writeArgs(e.Args)
		w.b.WriteByte(')')

	case schema.ExprBinary:
		w.writeBinary(e)

	case schema.ExprGrouping:
		// GROUPING() is real here — SQL Server has GROUPING SETS, CUBE and
		// ROLLUP in full, so unlike MySQL there is nothing to refuse alongside
		// it. See aggregate.go.
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
		// The cast is load-bearing in a way MySQL's is not. SQL Server's `/`
		// follows the operand types: two INTs divide as INTEGERS, so `total /
		// count` silently truncates — the same expression MySQL evaluates as a
		// decimal. The declaration says what scale it wants, so the left
		// operand is widened to a decimal of that scale and the result rounded
		// to it.
		scale := e.Type.Scale
		w.b.WriteString("ROUND(CAST(")
		w.writeExpr(e.Args[0])
		w.b.WriteString(" AS DECIMAL(38, ")
		w.b.WriteString(strconv.Itoa(scale))
		w.b.WriteString("))")
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
		w.fail(fmt.Errorf("compile/mssql: no lowering for arithmetic operator %v", op))
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

// ErrNoNullsPlacement is why a window ordering with an explicit NULLS placement
// cannot be lowered.
//
// SQL Server has no NULLS FIRST / NULLS LAST anywhere an ORDER BY appears,
// inside OVER() included. A plain read can ignore that, because its own null
// ordering — first ascending, last descending — happens to be exactly the two
// placements storm can ask for. A WINDOW ordering cannot: it is asked for
// explicitly, and dropping it would reorder the frame the aggregate reads.
var ErrNoNullsPlacement = errors.New(
	"compile/mssql: SQL Server has no NULLS FIRST / NULLS LAST, and a window's ordering " +
		"decides which rows the frame covers — dropping the placement would change the " +
		"result rather than its spelling")

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
			if t.NullsFirst != nil {
				w.fail(ErrNoNullsPlacement)
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
