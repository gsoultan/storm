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
	// ExprParam is a bound parameter: a value the CALL supplies, numbered at
	// generate time. Only a union has these — every other read binds through
	// the token stream, which a union has no call-site predicates to build.
	ExprParam
	// ExprBinary is arithmetic over two expressions. The OPERATOR is named
	// abstractly and spelled by the back end, like everything else here: `/`
	// does not mean the same thing in every dialect, which is exactly why the
	// IR must not carry the character.
	ExprBinary
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
	// Arith is the operator, for ExprBinary.
	Arith ArithOp
	// Param is the zero-based parameter index, for ExprParam. It is rendered
	// as $(Param+1), and the same parameter used in two branches renders as
	// the same placeholder — PostgreSQL allows that, and it keeps one declared
	// parameter to one argument at the call.
	Param int
	// Args are the operands.
	Args []Expr
	// Lit is a declaration-time literal, already in its Go form.
	Lit Literal

	// Distinct is count(DISTINCT x). Only valid on ExprAgg, and only
	// alongside a nil Over: PostgreSQL rejects an ordered-set aggregate with
	// DISTINCT over a window, so allowing both would emit SQL that parses in
	// storm and fails at the server.
	Distinct bool
	// Filter is an aggregate's FILTER (WHERE …). Only valid on ExprAgg.
	Filter *Cond
	// Over is the window. Valid on ExprAgg and required on ExprWindow.
	Over *Window

	// Type and Nullable are resolved when the expression is built, so codegen
	// never has to re-derive them and the two cannot disagree.
	Type     Type
	Nullable bool
}

// DivScaleDefault is the scale Div rounds to when the declaration does not
// say. Six places is past what any ratio or percentage reports and well inside
// what a Decimal holds; DivScale takes it explicitly when money needs more.
const DivScaleDefault = 6

// DivScaleMax is the largest scale a division may declare. Past this a result
// with any integer part at all cannot fit eighteen significant digits, so the
// value would decode as a range error rather than a number.
const DivScaleMax = 12

// ArithOp is an arithmetic operator, named rather than spelled.
type ArithOp string

// The four operators storm can emit. Modulo and exponentiation are absent
// deliberately: neither has appeared in a real declaration, and every operator
// here is a result-type rule that has to be right in five dialects.
const (
	ArithAdd ArithOp = "add"
	ArithSub ArithOp = "sub"
	ArithMul ArithOp = "mul"
	ArithDiv ArithOp = "div"
)

// BinaryResult is the result type of an arithmetic expression.
//
// The whole content of this function is division. PostgreSQL's `/` on two
// integers is INTEGER division — `count(a) / count(b)` is 0 or 1, never 0.6 —
// and a ratio is the reason anyone wants arithmetic in a declaration at all.
// Silently truncating it would be precisely the class of defect this library
// exists to remove, so integer division resolves to numeric and the back end
// renders the cast that makes that true.
//
// The other three widen within the numeric family and refuse to cross it:
// adding text to a number is a mistake, not a conversion.
func BinaryResult(op ArithOp, in []Type, divScale int) (Type, error) {
	if len(in) != 2 {
		return Type{}, fmt.Errorf("wants exactly two operands, got %d", len(in))
	}
	for _, t := range in {
		if !numericFamily(t.Name) {
			return Type{}, fmt.Errorf(
				"arithmetic wants numbers; %s is not one", t.Name)
		}
	}
	if op == ArithDiv {
		// Always numeric, always at a DECLARED scale, whatever the operands.
		//
		// Two reasons, and both are answers the operand types cannot give.
		// Integer division truncates, so `paid / orders` would be 0 or 1.
		// And PostgreSQL's numeric division picks its own scale — 1/4 comes
		// back as 0.25000000000000000000, twenty digits — which overflows the
		// eighteen significant digits a Decimal holds. The scale is part of
		// the declaration for the same reason a column's is.
		return Type{Name: TypeNumeric, Scale: divScale}, nil
	}
	return unifyAll("arithmetic", in)
}

// integerFamily reports the types whose division truncates.
func integerFamily(n string) bool {
	switch n {
	case TypeInt2, TypeInt4, TypeInt8:
		return true
	}
	return false
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
	// Frame narrows the rows the window function sees within its partition.
	// Nil is PostgreSQL's default — RANGE from the start of the partition to
	// the CURRENT ROW — which is not what most people picture and is why
	// last_value() without a frame returns the current row.
	Frame *Frame
}

// FrameKind selects how a frame's bounds are counted.
type FrameKind uint8

const (
	// FrameRows counts ROWS: "the three rows before this one", whatever their
	// values.
	FrameRows FrameKind = iota
	// FrameRange counts RANGE: peers — rows the ORDER BY cannot tell apart —
	// are one unit.
	FrameRange
)

// BoundKind is where a frame edge sits.
type BoundKind uint8

const (
	UnboundedPreceding BoundKind = iota
	Preceding
	CurrentRow
	Following
	UnboundedFollowing
)

// FrameBound is one edge of a frame.
type FrameBound struct {
	Kind BoundKind
	// N is the offset, for Preceding and Following.
	N int
}

// Rank orders the bounds along the partition, which is what makes "start after
// end" a decidable question rather than a server error.
func (b FrameBound) Rank() int { return int(b.Kind) }

// Frame is a window frame.
type Frame struct {
	Kind       FrameKind
	Start, End FrameBound
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

// DateTruncUnits is the field names date_trunc accepts.
//
// An allow-list for the same reason the function list is one: the unit is a
// STRING, so a typo is not a compile error, and PostgreSQL's answer to
// `date_trunc('dya', ...)` is a runtime error on a statement that was fixed at
// generate time. Everything else about a declared aggregation is checked
// before it can run; this was the one string that was not.
var DateTruncUnits = map[string]bool{
	"microseconds": true, "milliseconds": true, "second": true, "minute": true,
	"hour": true, "day": true, "week": true, "month": true, "quarter": true,
	"year": true, "decade": true, "century": true, "millennium": true,
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
			return unifyAll("coalesce", in)
		},
		// The point of coalesce: NOT nullable if any later argument is a
		// non-null literal. Resolved where it is built, which knows that.
	},
	"nullif": {
		Args: 2,
		Result: func(in []Type) (Type, error) {
			// Deliberately NOT exact-match. `nullif(amount, 0)` is the canonical
			// division-by-zero guard and the reason this function exists; PostgreSQL
			// casts the literal, and requiring numeric(0) at the call site would
			// make the documented use case unwritable.
			return unifyAll("nullif", in)
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

// unifyAll finds one type every argument can be read as.
//
// Exact equality is too strict for the expressions people actually write:
// `coalesce(balance, 0)` and `nullif(amount, 0)` mix a numeric column with an
// integer literal, PostgreSQL casts the literal, and refusing them would make
// the documented use of both functions unwritable. Anything outside one family
// is still refused — mixing text with a number is a mistake, not a cast.
// UnifyUnion is the type a union column takes when two branches disagree about
// it. text next to varchar(300) is text, int4 next to int8 is int8 — the same
// widening PostgreSQL does for a UNION, said out loud so the generated row
// type carries the type the server will actually send.
func UnifyUnion(a, b Type) (Type, error) {
	return unifyAll("a union column", []Type{a, b})
}

func unifyAll(fn string, in []Type) (Type, error) {
	out := in[0]
	for _, t := range in[1:] {
		if t.Name == out.Name {
			continue
		}
		if numericFamily(out.Name) && numericFamily(t.Name) {
			// Widen to the more capacious of the two, so a numeric column next
			// to an int literal stays numeric rather than narrowing to int.
			if numericRank(t.Name) > numericRank(out.Name) {
				out = t
			}
			continue
		}
		if textFamily(out.Name) && textFamily(t.Name) {
			out = Type{Name: TypeText}
			continue
		}
		return Type{}, fmt.Errorf("%s mixes %s and %s, which PostgreSQL will not unify",
			fn, out.Name, t.Name)
	}
	return out, nil
}

func numericFamily(n string) bool {
	switch n {
	case TypeInt2, TypeInt4, TypeInt8, TypeNumeric, TypeFloat4, TypeFloat8:
		return true
	}
	return false
}

// numericRank orders the numeric types by how much they can hold, so unifying
// picks the one that cannot lose the other.
func numericRank(n string) int {
	switch n {
	case TypeInt2:
		return 1
	case TypeInt4:
		return 2
	case TypeInt8:
		return 3
	case TypeFloat4:
		return 4
	case TypeFloat8:
		return 5
	case TypeNumeric:
		return 6
	}
	return 0
}

func textFamily(n string) bool { return n == TypeText || n == TypeVarchar }
