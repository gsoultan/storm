package schema

import (
	"fmt"
	"strconv"
	"strings"
)

// ExprKind is what an expression node is.
type ExprKind uint8

const (
	// ExprCol is a bare column reference.
	ExprCol ExprKind = iota
	// ExprFunc is a scalar function call over other expressions.
	ExprFunc
	// ExprLit is a literal fixed at DECLARATION time.
	//
	// Declared, never bound: it comes from a Go value in a Schema method, so
	// there is no runtime input here and nothing to parameterise. Rendering it
	// into the statement text is what lets the whole aggregation stay one
	// cached, prepared statement instead of one per literal.
	ExprLit
	// ExprStar is `*`, valid only as count's argument.
	ExprStar
	// ExprAgg is an aggregate call: count/sum/avg/min/max over an expression,
	// optionally filtered, optionally windowed.
	ExprAgg
	// ExprWindow is a window-only function — row_number, rank, lag — which has
	// no meaning without an OVER clause.
	ExprWindow
	// ExprGrouping is GROUPING(cols...), which distinguishes a subtotal row's
	// NULL from a NULL that was in the data.
	ExprGrouping
)

// Expr is a typed expression tree.
//
// It is a LOGICAL plan node, not SQL text: `compile/pgsql` renders it, and
// nothing outside that package spells an operator or a function name. Same rule
// the read path already follows (ADR-0004, and boundaries.sh enforces it).
type Expr struct {
	Kind ExprKind

	// Col is the column, for ExprCol.
	Col string
	// Tbl qualifies the column, for a join or a CTE reference. Empty means the
	// query's own table, which is every single-table read.
	Tbl string
	// Fn is the function or aggregate name, for ExprFunc/ExprAgg/ExprWindow.
	Fn string
	// Args are the operands.
	Args []Expr
	// Lit is a declaration-time literal, already in its Go form.
	Lit Literal

	// Filter is an aggregate's FILTER (WHERE …). Only valid on ExprAgg.
	Filter *Cond
	// Over is the window. Valid on ExprAgg and required on ExprWindow.
	Over *Window

	// Type and Nullable are resolved when the expression is built, so codegen
	// never has to re-derive them and the two cannot disagree.
	Type     Type
	Nullable bool
}

// Literal is a declaration-time constant.
type Literal struct {
	// Kind is the storm type name the literal carries.
	Kind string
	// S, I, F hold the value. Only the one matching Kind is meaningful.
	S string
	I int64
	F float64
	B bool
}

// Window is an OVER clause.
type Window struct {
	PartitionBy []Expr
	OrderBy     []OrderTerm
}

// OrderTerm is one ORDER BY element inside a window.
type OrderTerm struct {
	Expr Expr
	Desc bool
	// NullsFirst is meaningful only when explicitly set; PostgreSQL's default
	// is NULLS LAST for ASC and NULLS FIRST for DESC.
	NullsFirst *bool
}

// CondKind is what a condition node is.
type CondKind uint8

const (
	CondCmp CondKind = iota
	CondAnd
	CondOr
	CondNot
	CondIsNull
	CondIsNotNull
)

// Cond is a DECLARED predicate: fixed at generation time, rendered into the
// statement.
//
// Deliberately separate from the runtime predicate machinery. A call-site
// `Where` is dynamic — it varies per call, so it is a token stream spliced into
// a cached statement with bound arguments. A FILTER or a HAVING is part of the
// declaration and never varies, so it belongs in the text. Sharing one
// representation would have forced the dynamic machinery onto something that is
// not dynamic, or the static one onto something that is.
type Cond struct {
	Kind CondKind
	// Left and Right are the operands of a comparison.
	Left, Right Expr
	// Op is the comparison operator, for CondCmp.
	Op CmpOp
	// Args are the operands of And/Or/Not.
	Args []Cond
}

// CmpOp is a comparison in a declared predicate.
type CmpOp string

const (
	OpEq  CmpOp = "="
	OpNe  CmpOp = "<>"
	OpLt  CmpOp = "<"
	OpLte CmpOp = "<="
	OpGt  CmpOp = ">"
	OpGte CmpOp = ">="
)

// ---- function catalogue -----------------------------------------------------

// FuncSig describes a scalar function storm can emit.
type FuncSig struct {
	// Args is the number of operands, or -1 for variadic.
	Args int
	// Result computes the result type from the argument types.
	Result func(in []Type) (Type, error)
	// NullableIfAnyArgIs makes the result nullable when any argument is. Most
	// scalar functions are null-propagating; the exceptions say so.
	NullableIfAnyArgIs bool
}

// Funcs is the allow-list of scalar functions.
//
// An allow-list, and a small one. An open `storm.Raw("whatever(x)")` would
// reintroduce the string-typed surface the library exists to remove, and would
// make the result type unknowable — which is what the generated row's field
// type depends on. Adding one is a deliberate act with a test.
var Funcs = map[string]FuncSig{
	"date_trunc": {
		Args: 2,
		Result: func(in []Type) (Type, error) {
			// date_trunc(text, timestamptz) → timestamptz
			if len(in) != 2 || in[0].Name != TypeText {
				return Type{}, fmt.Errorf("date_trunc(unit, ts) wants a text unit")
			}
			switch in[1].Name {
			case TypeTimestamptz, TypeTimestamp, TypeDate:
				return Type{Name: TypeTimestamptz}, nil
			}
			return Type{}, fmt.Errorf("date_trunc has no meaning for %s", in[1].Name)
		},
		NullableIfAnyArgIs: true,
	},
	"coalesce": {
		Args: -1,
		Result: func(in []Type) (Type, error) {
			if len(in) < 2 {
				return Type{}, fmt.Errorf("coalesce wants at least two arguments")
			}
			for _, t := range in[1:] {
				if t.Name != in[0].Name {
					return Type{}, fmt.Errorf(
						"coalesce mixes %s and %s — every argument must be the same type",
						in[0].Name, t.Name)
				}
			}
			return in[0], nil
		},
		// The point of coalesce: NOT nullable if any later argument is a
		// non-null literal. Resolved where it is built, which knows that.
	},
	"nullif": {
		Args: 2,
		Result: func(in []Type) (Type, error) {
			if in[0].Name != in[1].Name {
				return Type{}, fmt.Errorf("nullif mixes %s and %s", in[0].Name, in[1].Name)
			}
			return in[0], nil
		},
		// ALWAYS nullable — that is its whole job, and it is the
		// division-by-zero guard the design doc leans on.
	},
	"lower": {Args: 1, Result: sameText, NullableIfAnyArgIs: true},
	"upper": {Args: 1, Result: sameText, NullableIfAnyArgIs: true},
	"abs":   {Args: 1, Result: sameNumeric, NullableIfAnyArgIs: true},
}

func sameText(in []Type) (Type, error) {
	switch in[0].Name {
	case TypeText, TypeVarchar:
		return Type{Name: TypeText}, nil
	}
	return Type{}, fmt.Errorf("wants text, got %s", in[0].Name)
}

func sameNumeric(in []Type) (Type, error) {
	switch in[0].Name {
	case TypeInt2, TypeInt4, TypeInt8, TypeNumeric, TypeFloat4, TypeFloat8:
		return in[0], nil
	}
	return Type{}, fmt.Errorf("wants a number, got %s", in[0].Name)
}

// ---- window functions -------------------------------------------------------

// WindowSig describes a window-only function.
type WindowSig struct {
	// Args is how many operands it takes beyond the window itself.
	Args int
	// Result computes the result type. `in` is empty for rank-like functions.
	Result func(in []Type) (Type, error)
	// AlwaysNullable marks lag/lead/first_value, which return NULL at the edge
	// of a partition however non-null the column is.
	AlwaysNullable bool
	// NeedsOrder marks functions that are meaningless without an ORDER BY in
	// the window — a rank with no ordering ranks by nothing.
	NeedsOrder bool
}

// WindowFuncs is the allow-list of window-only functions.
var WindowFuncs = map[string]WindowSig{
	"row_number":   {Args: 0, Result: bigint, NeedsOrder: true},
	"rank":         {Args: 0, Result: bigint, NeedsOrder: true},
	"dense_rank":   {Args: 0, Result: bigint, NeedsOrder: true},
	"percent_rank": {Args: 0, Result: float8, NeedsOrder: true},
	"cume_dist":    {Args: 0, Result: float8, NeedsOrder: true},
	"lag":          {Args: 1, Result: firstArg, AlwaysNullable: true, NeedsOrder: true},
	"lead":         {Args: 1, Result: firstArg, AlwaysNullable: true, NeedsOrder: true},
	"first_value":  {Args: 1, Result: firstArg, AlwaysNullable: true, NeedsOrder: true},
	"last_value":   {Args: 1, Result: firstArg, AlwaysNullable: true, NeedsOrder: true},
}

func bigint([]Type) (Type, error) { return Type{Name: TypeInt8}, nil }
func float8([]Type) (Type, error) { return Type{Name: TypeFloat8}, nil }
func firstArg(in []Type) (Type, error) {
	if len(in) != 1 {
		return Type{}, fmt.Errorf("wants exactly one argument")
	}
	return in[0], nil
}

// ---- literals ---------------------------------------------------------------

// SQL renders a literal. Declaration-time only: there is no runtime input here,
// but a string still has to be escaped or a quote in a declared constant would
// break the statement.
func (l Literal) SQL() string {
	switch l.Kind {
	case TypeText, TypeVarchar:
		return "'" + strings.ReplaceAll(l.S, "'", "''") + "'"
	case TypeInt2, TypeInt4, TypeInt8:
		return strconv.FormatInt(l.I, 10)
	case TypeFloat4, TypeFloat8:
		return strconv.FormatFloat(l.F, 'g', -1, 64)
	case TypeNumeric:
		// Kept as text and cast, so a decimal literal never travels through a
		// float on its way into the statement.
		return "'" + l.S + "'::numeric"
	case TypeBool:
		if l.B {
			return "true"
		}
		return "false"
	}
	return "NULL"
}
