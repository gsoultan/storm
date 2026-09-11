package mysql

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Expression rendering for MySQL.
//
// This is NOT compile/pgsql's renderer with the quoting swapped. Four of its
// decisions are PostgreSQL facts that its own comments say so, and one is a
// hazard that only exists on a bare-placeholder back end:
//
//   - `FILTER (WHERE …)` on an aggregate. MySQL has no such clause. The usual
//     rewrite is `SUM(CASE WHEN … THEN x END)`, which is NOT a spelling — it
//     changes what `count(*)` counts, and getting it wrong is a wrong number
//     with no error. Refused until it is done deliberately.
//   - The division cast. pgsql's note: "MySQL's `/` already yields a decimal,
//     and its back end must not inherit a cast it does not need." It still
//     rounds to the declared scale, because MySQL picks its own otherwise
//     (div_precision_increment, 4 by default).
//   - `::numeric`. MySQL spells a cast `CAST(x AS DECIMAL(…))`.
//   - A repeated parameter. PostgreSQL numbers placeholders, so `$1` used in
//     two union branches binds ONE value. MySQL binds by position, so the same
//     value would have to be passed twice — a different arity than the caller
//     was generated against. Refused rather than silently doubled.
//   - `arithOp` falling through to `" ? "`. Harmless in PostgreSQL, where `?`
//     is an operator; on MySQL it is a BOUND PARAMETER, so an operator storm
//     does not know would silently add one to the statement's arity.
//
// Errors accumulate on the writer rather than being returned from every call,
// because these render into a strings.Builder deep inside a tree walk and a
// returned error at each node would be noise at fifty call sites.

// ErrNoFilterClause is why an aggregate with a FILTER cannot be lowered.
var ErrNoFilterClause = errors.New(
	"compile/mysql: MySQL has no FILTER (WHERE …) clause on an aggregate. The rewrite is " +
		"SUM(CASE WHEN … THEN … END), which changes what the aggregate counts rather than " +
		"how it is spelled, so storm will not do it silently")

// ErrRepeatedParam is why a declared parameter used twice cannot be lowered.
var ErrRepeatedParam = errors.New(
	"compile/mysql: MySQL binds parameters by position, so a declared parameter used more " +
		"than once would have to be passed once per USE, not once per parameter — a different " +
		"arity than the caller was generated against")

// exprWriter renders expressions, carrying the first thing it could not lower.
type exprWriter struct {
	b    strings.Builder
	err  error
	seen map[int]bool // declared parameters already rendered
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
		// Bare. Position binds the value, so the same parameter used twice is
		// two bindings — which the caller was not generated for.
		if w.seen == nil {
			w.seen = map[int]bool{}
		}
		if w.seen[e.Param] {
			w.fail(fmt.Errorf("%w (parameter %d)", ErrRepeatedParam, e.Param+1))
		}
		w.seen[e.Param] = true
		w.b.WriteString(Placeholder)

	case schema.ExprFunc:
		w.b.WriteString(e.Fn)
		w.b.WriteByte('(')
		w.writeArgs(e.Args)
		w.b.WriteByte(')')

	case schema.ExprBinary:
		w.writeBinary(e)

	case schema.ExprGrouping:
		// GROUPING() belongs to GROUPING SETS / ROLLUP, and MySQL has only
		// WITH ROLLUP. Refused with the grouping form itself, in GroupBy.
		w.b.WriteString("GROUPING(")
		w.writeArgs(e.Args)
		w.b.WriteByte(')')

	case schema.ExprAgg:
		// No round() wrapper here. PostgreSQL needs one because its numeric
		// division picks a scale wide enough to overflow a Decimal; MySQL's
		// avg() returns a DECIMAL whose scale it derives from the input, and
		// wrapping it would round twice.
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
		// No cast: MySQL's `/` already yields a decimal, so the operands need
		// no help. It still ROUNDS, because MySQL derives its own scale from
		// div_precision_increment (4 by default) and the declaration said
		// otherwise.
		w.b.WriteString("ROUND(")
		w.writeExpr(e.Args[0])
		w.writeArith(e.Arith)
		w.writeExpr(e.Args[1])
		w.b.WriteString(", ")
		w.b.WriteString(strconv.Itoa(e.Type.Scale))
		w.b.WriteByte(')')
		return
	}
	w.b.WriteByte('(')
	w.writeExpr(e.Args[0])
	w.writeArith(e.Arith)
	w.writeExpr(e.Args[1])
	w.b.WriteByte(')')
}

// writeArith spells the operator, and refuses one it does not know.
//
// pgsql's falls through to " ? ", which is an OPERATOR in PostgreSQL and a
// BOUND PARAMETER here. An unknown op would silently add one to the statement's
// arity, and the caller would bind the wrong values to everything after it.
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
		w.fail(fmt.Errorf("compile/mysql: no lowering for arithmetic operator %v", op))
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
// MySQL has no NULLS FIRST / NULLS LAST anywhere an ORDER BY appears, inside
// OVER() included. A plain read can ignore that, because MySQL's own null
// ordering — first ascending, last descending — happens to be exactly the two
// placements storm can ask for. A WINDOW ordering cannot: it is asked for
// explicitly, and dropping it would reorder the frame the aggregate reads.
var ErrNoNullsPlacement = errors.New(
	"compile/mysql: MySQL has no NULLS FIRST / NULLS LAST, and a window's ordering " +
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

// writeFrame renders ROWS/RANGE BETWEEN. MySQL 8 has window frames, and the
// shape is standard.
//
// Always BETWEEN, never the one-bound shorthand, for the reason PostgreSQL's
// is: `ROWS 3 PRECEDING` means `BETWEEN 3 PRECEDING AND CURRENT ROW`, which is
// a rule people misremember in both directions.
func (w *exprWriter) writeFrame(f schema.Frame) {
	if f.Kind == schema.FrameRange {
		w.b.WriteString(" RANGE BETWEEN ")
	} else {
		w.b.WriteString(" ROWS BETWEEN ")
	}
	w.writeBound(f.Start)
	w.b.WriteString(" AND ")
	w.writeBound(f.End)
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
