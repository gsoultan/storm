package storm

import (
	"errors"
	"fmt"
	"time"

	"github.com/gsoultan/storm/schema"
)

// Exprs is the declaration-time expression vocabulary, reached through the
// `*Aggregates` or `*Joins` value a declaration is handed.
//
// Methods rather than package functions, and deliberately. These were
// `storm.Eq`, `storm.And`, `storm.Col` and nineteen more at the top level,
// where they sat beside the generated query API meaning something different:
// `order.Status.Eq(x)` filters rows at run time, `storm.Eq(&o.Status, x)`
// described a filter at declaration time. Two `Eq`s in scope with different
// semantics is a question every reader has to answer once. Hanging them off the
// builder answers it structurally — a declaration constructor cannot be reached
// from a query, because the builder is not in scope there.
type Exprs struct{}

// Term is an expression inside a declaration: a column, a literal, a scalar
// function, or a reference to another declared output.
//
// Resolved and TYPED at generation time, which is the point — the generated
// row's field type is whatever the Term turns out to be.
//
// Field pointers are accepted anywhere a Term is, so the common case reads as
// itself: a.DateTrunc("day", &o.PlacedAt).
type Term struct {
	kind  schema.ExprKind
	fp    any // field pointer, resolved against the table
	out   string
	fn    string
	args  []Term
	lit   schema.Literal
	arith schema.ArithOp
	scale int
	// param is the 1-based union parameter index; 0 is "not a parameter", so
	// the zero Term stays meaningless rather than meaning parameter 0.
	param int
	err   error
}

// Col is an explicit column reference. Rarely needed: a field pointer is
// accepted directly wherever a Term is.
func (Exprs) Col(fieldPtr any) Term { return Term{kind: schema.ExprCol, fp: fieldPtr} }

// star is `*`. Unexported because there is no call site for it: Count already
// means count(*), CountOf takes the column, and every other position refuses a
// star.
func star() Term { return Term{kind: schema.ExprStar} }

// Lit is a declaration-time constant.
//
// Rendered into the statement rather than bound, because it comes from the
// declaration and never varies — which is what keeps a filtered aggregate one
// cached statement instead of one per value.
func (Exprs) Lit(v any) Term { return lit(v) }

func lit(v any) Term {
	t := Term{kind: schema.ExprLit}
	switch x := v.(type) {
	case string:
		t.lit = schema.Literal{Kind: schema.TypeText, S: x}
	case int:
		t.lit = schema.Literal{Kind: schema.TypeInt8, I: int64(x)}
	case int32:
		t.lit = schema.Literal{Kind: schema.TypeInt4, I: int64(x)}
	case int64:
		t.lit = schema.Literal{Kind: schema.TypeInt8, I: x}
	case float64:
		t.lit = schema.Literal{Kind: schema.TypeFloat8, F: x}
	case bool:
		t.lit = schema.Literal{Kind: schema.TypeBool, B: x}
	case Decimal:
		t.lit = schema.Literal{Kind: schema.TypeNumeric, S: x.String()}
	case time.Time:
		t.lit = schema.Literal{Kind: schema.TypeTimestamptz, S: x.UTC().Format(time.RFC3339Nano)}
	default:
		// An enum declared as a named string type is the common case here.
		if s, ok := asString(v); ok {
			t.lit = schema.Literal{Kind: schema.TypeText, S: s}
			break
		}
		t.err = fmt.Errorf("Lit: %T is not a literal storm can render", v)
	}
	return t
}

// DateTrunc buckets a timestamp — the reason grouping takes an expression at
// all. `unit` is a PostgreSQL field name: "hour", "day", "month", "year".
func (Exprs) DateTrunc(unit string, ts any) Term {
	return Term{kind: schema.ExprFunc, fn: "date_trunc", args: []Term{lit(unit), toTerm(ts)}}
}

// Add, Sub, Mul and Div are arithmetic over two terms — the ratio a report
// asks for, written where the division is.
//
// Div on two integers resolves to NUMERIC, not to an integer. PostgreSQL's `/`
// truncates, so `Div(paid, total)` over two counts would otherwise be 0 for
// every group that is not entirely paid — a plausible-looking wrong answer,
// which is the only kind that matters.
//
// Division by zero is still an error at the server, and NullIf is the guard:
//
//	a.Div(recent, a.NullIf(prior, a.Lit(0)))   // NULL, not a failed query
func (Exprs) Add(l, r any) Term { return arith(schema.ArithAdd, l, r) }
func (Exprs) Sub(l, r any) Term { return arith(schema.ArithSub, l, r) }
func (Exprs) Mul(l, r any) Term { return arith(schema.ArithMul, l, r) }
func (Exprs) Div(l, r any) Term {
	t := arith(schema.ArithDiv, l, r)
	t.scale = schema.DivScaleDefault
	return t
}

// DivScale is Div with the scale said out loud — money that needs more than six
// places, or a percentage that wants two.
func (Exprs) DivScale(l, r any, scale int) Term {
	if scale < 0 || scale > schema.DivScaleMax {
		return Term{err: fmt.Errorf(
			"division scale %d is outside 0..%d — past that a result with any "+
				"integer part cannot fit the eighteen significant digits a Decimal holds",
			scale, schema.DivScaleMax)}
	}
	t := arith(schema.ArithDiv, l, r)
	t.scale = scale
	return t
}

func arith(op schema.ArithOp, l, r any) Term {
	return Term{kind: schema.ExprBinary, arith: op, args: toTerms([]any{l, r})}
}

// Lower and Upper case-fold text. Useful in a grouping expression, where
// "Ada" and "ada" are one group or two and the answer has to be said.
func (Exprs) Lower(x any) Term {
	return Term{kind: schema.ExprFunc, fn: "lower", args: toTerms([]any{x})}
}
func (Exprs) Upper(x any) Term {
	return Term{kind: schema.ExprFunc, fn: "upper", args: toTerms([]any{x})}
}

// Coalesce returns the first non-null argument.
func (Exprs) Coalesce(args ...any) Term {
	return Term{kind: schema.ExprFunc, fn: "coalesce", args: toTerms(args)}
}

// NullIf returns NULL when the two arguments are equal — the division-by-zero
// guard, written where the division is rather than in a comment above it.
func (Exprs) NullIf(a, b any) Term {
	return Term{kind: schema.ExprFunc, fn: "nullif", args: toTerms([]any{a, b})}
}

// Abs is the absolute value.
func (Exprs) Abs(x any) Term {
	return Term{kind: schema.ExprFunc, fn: "abs", args: toTerms([]any{x})}
}

// Grouping reports, per row, whether a grouping set aggregated over these
// columns. It is how a ROLLUP's subtotal NULL is told apart from a NULL that
// was in the data — without it a subtotal row and a real NULL group are
// indistinguishable, which is a wrong answer that looks like a right one.
func (Exprs) Grouping(fieldPtrs ...any) Term {
	return Term{kind: schema.ExprGrouping, args: toTerms(fieldPtrs)}
}

func toTerm(v any) Term {
	switch x := v.(type) {
	case Term:
		return x
	case Out:
		// A declared output, referenced by the handle its declaration returned.
		return Term{out: x.name}
	}
	return Term{kind: schema.ExprCol, fp: v}
}

func toTerms(vs []any) []Term {
	out := make([]Term, 0, len(vs))
	for _, v := range vs {
		out = append(out, toTerm(v))
	}
	return out
}

// asString reads a named string type (an enum) so a declared literal can carry
// its value.
func asString(v any) (string, bool) {
	type stringer interface{ String() string }
	if s, ok := v.(stringer); ok {
		return s.String(), true
	}
	return reflectString(v)
}

// ---- conditions -------------------------------------------------------------

// Cond is a DECLARED predicate, used by Filter, Having and a join's Where.
//
// Distinct from a call-site Where, which is dynamic: that varies per call and
// is a token stream spliced into a cached statement with bound arguments. A
// declared predicate never varies, so it is rendered into the text.
type Cond struct {
	kind  schema.CondKind
	left  Term
	right Term
	op    schema.CmpOp
	args  []Cond
	err   error
}

// Eq and friends compare a column, a declared output or an expression against
// a literal.
func (Exprs) Eq(l, r any) Cond  { return cmp(schema.OpEq, l, r) }
func (Exprs) Ne(l, r any) Cond  { return cmp(schema.OpNe, l, r) }
func (Exprs) Lt(l, r any) Cond  { return cmp(schema.OpLt, l, r) }
func (Exprs) Lte(l, r any) Cond { return cmp(schema.OpLte, l, r) }
func (Exprs) Gt(l, r any) Cond  { return cmp(schema.OpGt, l, r) }
func (Exprs) Gte(l, r any) Cond { return cmp(schema.OpGte, l, r) }

func cmp(op schema.CmpOp, l, r any) Cond {
	left := toTerm(l)
	// A bare Go value on the right is a literal; a field pointer, Term or Out
	// is itself. This is the asymmetry every comparison has: `status = 'paid'`.
	var right Term
	switch x := r.(type) {
	case Term:
		right = x
	case Out:
		right = Term{out: x.name}
	default:
		if isFieldPtr(r) {
			right = Term{kind: schema.ExprCol, fp: r}
		} else {
			right = lit(r)
		}
	}
	return Cond{kind: schema.CondCmp, op: op, left: left, right: right}
}

// And, Or and Not compose conditions. Always parenthesised when rendered:
// AND/OR precedence is a classic source of silently wrong predicates and the
// brackets cost nothing.
func (Exprs) And(cs ...Cond) Cond { return Cond{kind: schema.CondAnd, args: cs} }
func (Exprs) Or(cs ...Cond) Cond  { return Cond{kind: schema.CondOr, args: cs} }
func (Exprs) Not(c Cond) Cond     { return Cond{kind: schema.CondNot, args: []Cond{c}} }

// IsNull and IsNotNull test for NULL, which `= NULL` does not.
func (Exprs) IsNull(x any) Cond    { return Cond{kind: schema.CondIsNull, left: toTerm(x)} }
func (Exprs) IsNotNull(x any) Cond { return Cond{kind: schema.CondIsNotNull, left: toTerm(x)} }

// ---- windows ----------------------------------------------------------------

// WindowSpec is an OVER clause under construction.
type WindowSpec struct {
	partition []Term
	order     []windowOrder
	frame     *schema.Frame
	err       error
}

type windowOrder struct {
	term Term
	desc bool
}

// Over starts a window.
func (Exprs) Over() *WindowSpec { return &WindowSpec{} }

// Rows frames the window by COUNTED ROWS: `Rows(a.Preceding(6), a.CurrentRow())`
// is a seven-row moving window, which is the moving average everyone wants and
// the default frame cannot express.
//
// Without a frame PostgreSQL uses RANGE from the partition start to the current
// row — so a running total is the default and a moving one is not, and
// last_value() reads the current row rather than the last.
func (w *WindowSpec) Rows(start, end FrameBound) *WindowSpec {
	return w.framed(schema.FrameRows, start, end)
}

// Range frames by PEERS — rows the ORDER BY cannot tell apart count as one.
//
// Offsets are refused here. `RANGE 7 PRECEDING` needs exactly one ORDER BY
// column of a type that can be subtracted, and the failure is a server error
// at the first call rather than anything storm could name; ROWS expresses the
// same intent with a rule that always holds. Use Range for the unbounded
// edges, which is what it is actually good for.
func (w *WindowSpec) Range(start, end FrameBound) *WindowSpec {
	if start.Kind == schema.Preceding || start.Kind == schema.Following ||
		end.Kind == schema.Preceding || end.Kind == schema.Following {
		w.err = errors.New(
			"RANGE with an offset needs a single ORDER BY column that can be " +
				"subtracted — use Rows for a counted frame")
		return w
	}
	return w.framed(schema.FrameRange, start, end)
}

func (w *WindowSpec) framed(k schema.FrameKind, start, end FrameBound) *WindowSpec {
	if w.frame != nil {
		w.err = errors.New("declares a frame twice")
		return w
	}
	// Decidable here rather than at the server. A frame whose start is after
	// its end selects nothing PostgreSQL will accept, and the message it gives
	// back names neither the declaration nor the window.
	if bad := badOrder(start, end); bad != "" {
		w.err = errors.New(bad)
		return w
	}
	w.frame = &schema.Frame{Kind: k, Start: schema.FrameBound(start), End: schema.FrameBound(end)}
	return w
}

// badOrder reports why a frame's edges are in the wrong order, or "".
func badOrder(start, end FrameBound) string {
	if start.Kind == schema.UnboundedFollowing {
		return "a frame cannot START at UNBOUNDED FOLLOWING"
	}
	if end.Kind == schema.UnboundedPreceding {
		return "a frame cannot END at UNBOUNDED PRECEDING"
	}
	if start.Rank() > end.Rank() {
		return "the frame's start is after its end"
	}
	// Both PRECEDING: the start must be the FURTHER back of the two, so its
	// offset is the larger. Both FOLLOWING: the reverse.
	if start.Kind == schema.Preceding && end.Kind == schema.Preceding && start.N < end.N {
		return "the frame's start is after its end: a larger PRECEDING offset is further back"
	}
	if start.Kind == schema.Following && end.Kind == schema.Following && start.N > end.N {
		return "the frame's start is after its end: a larger FOLLOWING offset is further forward"
	}
	return ""
}

// FrameBound is one edge of a window frame, built by the methods below.
type FrameBound = schema.FrameBound

// UnboundedPreceding, CurrentRow and UnboundedFollowing are the fixed frame
// edges; Preceding and Following take a count of rows.
func (Exprs) UnboundedPreceding() FrameBound {
	return FrameBound{Kind: schema.UnboundedPreceding}
}
func (Exprs) CurrentRow() FrameBound { return FrameBound{Kind: schema.CurrentRow} }
func (Exprs) UnboundedFollowing() FrameBound {
	return FrameBound{Kind: schema.UnboundedFollowing}
}
func (Exprs) Preceding(n int) FrameBound {
	return FrameBound{Kind: schema.Preceding, N: n}
}
func (Exprs) Following(n int) FrameBound {
	return FrameBound{Kind: schema.Following, N: n}
}

// PartitionBy restarts the window for each distinct value.
func (w *WindowSpec) PartitionBy(xs ...any) *WindowSpec {
	w.partition = append(w.partition, toTerms(xs)...)
	return w
}

// OrderByAsc and OrderByDesc order rows WITHIN the partition.
//
// Named this way rather than taking Asc/Desc because those already mean index
// ordering, and one word meaning two things in one declaration is how a wrong
// index gets built.
func (w *WindowSpec) OrderByAsc(xs ...any) *WindowSpec  { return w.orderBy(false, xs) }
func (w *WindowSpec) OrderByDesc(xs ...any) *WindowSpec { return w.orderBy(true, xs) }

func (w *WindowSpec) orderBy(desc bool, xs []any) *WindowSpec {
	for _, x := range xs {
		w.order = append(w.order, windowOrder{term: toTerm(x), desc: desc})
	}
	return w
}

// exportIdent is the exported Go field name for a column — schema.GoName, the
// single implementation shared with codegen. Spelling it twice is how
// `customer_id` became CustomerID in the generated struct and CustomerId in the
// declaration, and the scanner assigned to a field that did not exist.
func exportIdent(col string) string { return schema.GoName(col) }
