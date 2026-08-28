package storm

import (
	"fmt"
	"time"

	"github.com/gsoultan/storm/schema"
)

// Term is an expression inside a declared aggregation: a column, a literal, a
// scalar function, or an aggregate over those.
//
// It is not `Expr`, which is already the raw text of a CHECK constraint, and it
// is not the runtime predicate type either. A Term is resolved and TYPED at
// generation time — that is the whole point, because the generated row's field
// type is whatever the Term turns out to be.
//
// Field pointers are accepted anywhere a Term is, so the common case reads as
// itself: storm.DateTrunc("day", &o.PlacedAt).
type Term struct {
	kind schema.ExprKind
	fp   any // field pointer, resolved against the table
	// out names an already-declared output of the same aggregation, for
	// Having. PostgreSQL cannot see a SELECT alias in HAVING, so this expands
	// to the aggregate's whole expression rather than to its name.
	out  string
	fn   string
	args []Term
	lit  schema.Literal
	err  error
}

// Col is an explicit column reference. Rarely needed: a field pointer is
// accepted directly wherever a Term is.
func Col(fieldPtr any) Term { return Term{kind: schema.ExprCol, fp: fieldPtr} }

// Star is `*`, valid ONLY as the argument of Count. Anywhere else it is
// refused at declaration time: `HAVING * > 0` is not a query.
func Star() Term { return Term{kind: schema.ExprStar} }

// Out references an output this aggregation already declared, by name. It is
// how Having talks about an aggregate:
//
//	Count("Orders").
//	Having(storm.Gt(storm.Out("Orders"), 10))
//
// PostgreSQL cannot see a SELECT alias in HAVING — aliases are resolved after
// grouping — so this expands to `count(*) > 10`, which is what it means.
func Out(name string) Term { return Term{out: name} }

// Lit is a declaration-time constant.
//
// Rendered into the statement rather than bound, because it comes from the
// declaration and never varies — which is what keeps a filtered aggregate one
// cached statement instead of one per value.
func Lit(v any) Term {
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
		t.err = fmt.Errorf("storm.Lit: %T is not a literal storm can render", v)
	}
	return t
}

// DateTrunc buckets a timestamp — the reason grouping takes an expression at
// all. `unit` is a PostgreSQL field name: "hour", "day", "month", "year".
func DateTrunc(unit string, ts any) Term {
	return Term{kind: schema.ExprFunc, fn: "date_trunc", args: []Term{Lit(unit), toTerm(ts)}}
}

// Coalesce returns the first non-null argument.
func Coalesce(args ...any) Term {
	return Term{kind: schema.ExprFunc, fn: "coalesce", args: toTerms(args)}
}

// NullIf returns NULL when the two arguments are equal — the division-by-zero
// guard, written where the division is rather than in a comment above it.
func NullIf(a, b any) Term {
	return Term{kind: schema.ExprFunc, fn: "nullif", args: toTerms([]any{a, b})}
}

// Abs is the absolute value.
func Abs(x any) Term { return Term{kind: schema.ExprFunc, fn: "abs", args: toTerms([]any{x})} }

// Grouping reports, per row, whether a grouping set aggregated over these
// columns. It is how a ROLLUP's subtotal NULL is told apart from a NULL that
// was in the data — without it a subtotal row and a real NULL group are
// indistinguishable, which is a wrong answer that looks like a right one.
func Grouping(fieldPtrs ...any) Term {
	return Term{kind: schema.ExprGrouping, args: toTerms(fieldPtrs)}
}

func toTerm(v any) Term {
	if t, ok := v.(Term); ok {
		return t
	}
	return Col(v)
}

func toTerms(vs []any) []Term {
	out := make([]Term, 0, len(vs))
	for _, v := range vs {
		out = append(out, toTerm(v))
	}
	return out
}

// asString reads a named string type (an enum) without importing reflect into
// the hot path — this runs once, at declaration time.
func asString(v any) (string, bool) {
	type stringer interface{ String() string }
	if s, ok := v.(stringer); ok {
		return s.String(), true
	}
	return reflectString(v)
}

// ---- conditions -------------------------------------------------------------

// Cond is a DECLARED predicate, used by FILTER and HAVING.
//
// Distinct from a call-site Where, which is dynamic: that varies per call and
// is a token stream spliced into a cached statement with bound arguments. A
// FILTER or HAVING is part of the declaration and never varies, so it is
// rendered into the text.
type Cond struct {
	kind  schema.CondKind
	left  Term
	right Term
	op    schema.CmpOp
	args  []Cond
	err   error
}

// Eq and friends compare a column (or expression) with a declared literal.
func Eq(l, r any) Cond  { return cmp(schema.OpEq, l, r) }
func Ne(l, r any) Cond  { return cmp(schema.OpNe, l, r) }
func Lt(l, r any) Cond  { return cmp(schema.OpLt, l, r) }
func Lte(l, r any) Cond { return cmp(schema.OpLte, l, r) }
func Gt(l, r any) Cond  { return cmp(schema.OpGt, l, r) }
func Gte(l, r any) Cond { return cmp(schema.OpGte, l, r) }

func cmp(op schema.CmpOp, l, r any) Cond {
	left := toTerm(l)
	// A bare Go value on the right is a literal; a field pointer or Term is
	// itself. This is the asymmetry every comparison has: `status = 'paid'`.
	right, ok := r.(Term)
	if !ok {
		if isFieldPtr(r) {
			right = Col(r)
		} else {
			right = Lit(r)
		}
	}
	return Cond{kind: schema.CondCmp, op: op, left: left, right: right}
}

// And, Or and Not compose conditions. Always parenthesised when rendered:
// AND/OR precedence is a classic source of silently wrong predicates and the
// brackets cost nothing.
func And(cs ...Cond) Cond { return Cond{kind: schema.CondAnd, args: cs} }
func Or(cs ...Cond) Cond  { return Cond{kind: schema.CondOr, args: cs} }
func Not(c Cond) Cond     { return Cond{kind: schema.CondNot, args: []Cond{c}} }

// IsNull and IsNotNull test for NULL, which `= NULL` does not.
func IsNull(x any) Cond    { return Cond{kind: schema.CondIsNull, left: toTerm(x)} }
func IsNotNull(x any) Cond { return Cond{kind: schema.CondIsNotNull, left: toTerm(x)} }

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
func Over() *WindowSpec { return &WindowSpec{} }

// PartitionBy restarts the window for each distinct value.
func (w *WindowSpec) PartitionBy(xs ...any) *WindowSpec {
	w.partition = append(w.partition, toTerms(xs)...)
	return w
}

// OrderByAsc and OrderByDesc order rows WITHIN the partition.
//
// Named this way rather than taking storm.Asc/storm.Desc because those already
// mean index ordering, and one word meaning two things in one declaration is
// how a wrong index gets built.
func (w *WindowSpec) OrderByAsc(xs ...any) *WindowSpec { return w.orderBy(false, xs) }
func (w *WindowSpec) OrderByDesc(xs ...any) *WindowSpec {
	return w.orderBy(true, xs)
}

func (w *WindowSpec) orderBy(desc bool, xs []any) *WindowSpec {
	for _, x := range xs {
		w.order = append(w.order, windowOrder{term: toTerm(x), desc: desc})
	}
	return w
}

// exportIdent is the exported Go field name for a column — schema.GoName, which
// is the single implementation shared with codegen. Spelling it twice is how
// `customer_id` became CustomerID in the generated struct and CustomerId in the
// declaration, and the scanner assigned to a field that did not exist.
func exportIdent(col string) string { return schema.GoName(col) }
