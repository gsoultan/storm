package schema

import "fmt"

// AggFunc is an aggregate storm can declare.
type AggFunc string

const (
	AggCount AggFunc = "count"
	AggSum   AggFunc = "sum"
	AggAvg   AggFunc = "avg"
	AggMin   AggFunc = "min"
	AggMax   AggFunc = "max"
)

// Aggregate is one named grouped read: a GROUP BY and the aggregates over it.
type Aggregate struct {
	// Name is the aggregate's name as declared, e.g. "RevenueByStatus". The
	// generated type is the model name plus this.
	Name string

	// By are the grouping expressions in declaration order, which is also the
	// order they appear in the row type and in ORDER BY. A bare column is the
	// common case; date_trunc('day', placed_at) is the reason this is an
	// expression rather than a column name.
	By []GroupTerm

	// Sets, when set, replaces a flat GROUP BY with GROUPING SETS / ROLLUP /
	// CUBE. Every grouping expression becomes NULLABLE in the row type: a
	// subtotal row carries NULL for the columns it aggregated over.
	Sets *GroupingSets

	// OrderBy, when set, replaces the default ordering — the grouping columns
	// in declaration order — with an explicit one over ANY declared output.
	//
	// It exists so a measure can be the sort key. "The ten products by
	// revenue" orders by a sum, and the sum is not a grouping column, so
	// without this the only orderings expressible were the ones the grouping
	// already gave: the whole result had to come back and be sorted by the
	// caller, which turns a LIMIT 10 into every group in the table.
	//
	// The terms name OUTPUTS, not expressions. PostgreSQL resolves a bare name
	// in ORDER BY against the select list first, so ordering by the alias and
	// ordering by the aggregate are the same plan — and the alias keeps a
	// grouping set's subtotal NULLs visible, which repeating the expression
	// would not.
	OrderBy []AggOrder

	// Terms are the aggregate and window expressions, in declaration order.
	Terms []AggregateTerm

	// Params are values the CALL supplies, in declaration order. They are
	// numbered $1..$k in the statement's fixed PREFIX, and the dynamic
	// predicates continue from k+1.
	//
	// A FILTER is part of the declaration, so it cannot say "since 30 days
	// ago" — that is relative to when the query runs. A parameter is the
	// narrow answer: the aggregation's shape is still one shape.
	Params []Param

	// Having is the declared predicate over the grouped rows. Unlike a
	// call-site Where it never varies, so it is rendered into the statement
	// rather than bound.
	Having *Cond
}

// GroupTerm is one grouping expression and the field it lands in.
type GroupTerm struct {
	Expr Expr
	// As is the Go field name. For a bare column it is derived from the column
	// name; for an expression the declaration has to supply one, because
	// date_trunc('day', placed_at) has no obvious field name.
	As string
}

// GroupingSetsKind selects the multi-level grouping form.
type GroupingSetsKind uint8

// AggOrder is one explicit ordering term of an aggregation: a declared
// output's name, and the direction.
type AggOrder struct {
	// As is the output's Go field name, which is also the SQL alias.
	As string
	// Desc orders descending. A report's headline measure is almost always
	// descending, which is why the declaration says which rather than
	// defaulting.
	Desc bool
}

const (
	// SetsExplicit is GROUPING SETS ((a,b),(a),()).
	SetsExplicit GroupingSetsKind = iota
	// SetsRollup is ROLLUP(a,b): the hierarchy plus a grand total.
	SetsRollup
	// SetsCube is CUBE(a,b): every combination.
	SetsCube
)

// GroupingSets is multi-level grouping in one pass — the answer to the N+1
// query per facet.
type GroupingSets struct {
	Kind GroupingSetsKind
	// Sets are the explicit grouping sets, as indexes into Aggregate.By.
	// Ignored for ROLLUP and CUBE, which use every By term in order.
	Sets [][]int
}

// AggregateTerm is one output expression and the field it lands in.
type AggregateTerm struct {
	// Expr is the whole expression: an aggregate, a windowed aggregate, a
	// window-only function, or GROUPING().
	Expr Expr
	// As is the Go field name in the generated row type. The SQL alias is
	// derived from it, so a rename moves both together.
	As string
}

// AggregateResult is what PostgreSQL returns for an aggregate over `in`.
//
// This function is the whole correctness question, because **the result type is
// not the input type** and guessing wrong corrupts the decode silently:
//
//	sum(int4)  → int8      widening, so a sum of ints cannot overflow int4
//	sum(int8)  → numeric   PostgreSQL will not risk an int8 overflow either
//	avg(int8)  → numeric   an average is not an integer
//	avg(float4)→ float8
//	min/max(T) → T
//	count(…)   → int8
//
// The second half matters as much: over zero rows every aggregate except count
// returns **NULL**, so the generated field must be nullable. Decoding that NULL
// into a plain int64 would read 0, and "no rows" would become "sums to zero" —
// a wrong answer with no error, which is the class storm treats as P0.
func AggregateResult(fn AggFunc, in Type) (out Type, nullable bool, err error) {
	switch fn {
	case AggCount:
		// count never returns NULL: no rows is 0 rows.
		return Type{Name: TypeInt8}, false, nil

	case AggMin, AggMax:
		// Order-based, so the type is preserved exactly — including numeric's
		// precision and scale, which a Decimal decode depends on.
		return in, true, nil

	case AggSum:
		switch in.Name {
		case TypeInt2, TypeInt4:
			return Type{Name: TypeInt8}, true, nil
		case TypeInt8:
			return Type{Name: TypeNumeric}, true, nil
		case TypeNumeric:
			// Precision grows with the row count and PostgreSQL does not
			// bound it. Carrying the declared precision forward would let
			// codegen's MaxNumericPrecision check pass on a column whose SUM
			// cannot fit a Decimal, so the result is deliberately
			// unconstrained: unbounded numeric decodes, or fails loudly with
			// ErrDecimalRange, but is never silently truncated.
			return Type{Name: TypeNumeric}, true, nil
		case TypeFloat4:
			return Type{Name: TypeFloat4}, true, nil
		case TypeFloat8:
			return Type{Name: TypeFloat8}, true, nil
		case TypeInterval:
			return Type{Name: TypeInterval}, true, nil
		}
		return Type{}, false, fmt.Errorf("sum() has no meaning for %s", in.Name)

	case AggAvg:
		switch in.Name {
		case TypeInt2, TypeInt4, TypeInt8, TypeNumeric:
			// Scaled, unlike sum, because avg DIVIDES and PostgreSQL's numeric
			// division picks its own scale — avg of one numeric(12,2) value
			// 123456789.12 comes back as 123456789.120000000000, twenty-one
			// significant digits, and a Decimal holds eighteen. Sum can be left
			// unbounded because its scale is the input's; an average's is not,
			// so it is bounded here and the back end rounds to it.
			return Type{Name: TypeNumeric, Scale: DivScaleDefault}, true, nil
		case TypeFloat4, TypeFloat8:
			// avg(float4) is float8, not float4: PostgreSQL accumulates in
			// double precision.
			return Type{Name: TypeFloat8}, true, nil
		case TypeInterval:
			return Type{Name: TypeInterval}, true, nil
		}
		return Type{}, false, fmt.Errorf("avg() has no meaning for %s", in.Name)
	}
	return Type{}, false, fmt.Errorf("unknown aggregate %q", fn)
}

// AggregatableMinMax reports whether PostgreSQL has a min()/max() aggregate
// for this type.
//
// An ALLOW-list, taken from `pg_aggregate` on a live server rather than
// reasoned about, because the surprises run the wrong way: `min(uuid)` does not
// exist, and neither does `max(bool)` — both look obviously orderable and
// neither has an aggregate. A deny-list would default a new type to "allowed"
// and turn that into `function min(uuid) does not exist` at runtime, from a
// query the developer declared months earlier. This defaults to refused, at
// declaration time, where the fix is one line away.
func AggregatableMinMax(in Type) bool {
	if in.Array || in.Enum {
		return false
	}
	switch in.Name {
	case TypeInt2, TypeInt4, TypeInt8, TypeNumeric, TypeFloat4, TypeFloat8,
		TypeText, TypeVarchar, TypeDate, TypeTimestamptz, TypeTimestamp,
		TypeTime, TypeInterval, TypeInet:
		return true
	}
	return false
}

// AggregatableSumAvg reports whether sum()/avg() exist for this type. The same
// allow-list reasoning: PostgreSQL has them for the numeric types and interval,
// and nothing else.
func AggregatableSumAvg(in Type) bool {
	if in.Array || in.Enum {
		return false
	}
	switch in.Name {
	case TypeInt2, TypeInt4, TypeInt8, TypeNumeric, TypeFloat4, TypeFloat8, TypeInterval:
		return true
	}
	return false
}
