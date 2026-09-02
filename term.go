package storm

import (
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
	kind schema.ExprKind
	fp   any // field pointer, resolved against the table
	out  string
	fn   string
	args []Term
	lit  schema.Literal
	err  error
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
}

type windowOrder struct {
	term Term
	desc bool
}

// Over starts a window.
func (Exprs) Over() *WindowSpec { return &WindowSpec{} }

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
