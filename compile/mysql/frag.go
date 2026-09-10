package mysql

// Operator lowering for MySQL.
//
// This is not pgsql's table with the placeholder swapped. Three groups differ
// in kind, and each is either lowered differently or deliberately absent:
//
//   - PostgreSQL's array, range and network operators have no MySQL
//     equivalent. compile/myddl already refuses those COLUMN types, and their
//     absence here is the same refusal on the read path, so the two cannot
//     disagree about what the dialect supports.
//   - `In` is the one that could have sunk M9. PostgreSQL lowers it to
//     `= ANY($1)`: ONE placeholder for a whole list, so the statement text does
//     not depend on how many values the caller passed. MySQL's `IN (?,?,?)`
//     has value-dependent arity, which makes the shape key a function of
//     request data rather than of the program — storm being an interpreter
//     with extra steps. ADR-0010 settled it on JSON_TABLE: one bound JSON
//     value, text independent of list length, and `Index lookup using ix_...`
//     still in the plan.
//   - jsonb containment becomes JSON_CONTAINS, which is a FUNCTION rather than
//     an operator, so it wraps the identifier instead of following it. That is
//     why Frag returns a prefix and a suffix rather than one string.
type frag struct{ a, b string }

var frags = map[string]frag{
	"Eq":    {" = " + Placeholder, ""},
	"NotEq": {" <> " + Placeholder, ""},
	"Gt":    {" > " + Placeholder, ""},
	"Gte":   {" >= " + Placeholder, ""},
	"Lt":    {" < " + Placeholder, ""},
	"Lte":   {" <= " + Placeholder, ""},
	"Like":  {" LIKE " + Placeholder, ""},
	"ILike": {" COLLATE utf8mb4_0900_ai_ci LIKE " + Placeholder, ""},

	// In and NotIn are NOT here: their lowering needs the column's SQL type.
	// See InFrag.

	// JSON containment. A function, not an operator: JSON_CONTAINS(col, ?).
	"JSONContains":    {"", ""}, // filled by init, which needs the wrap form
	"JSONContainedBy": {"", ""},

	"IsNull":    {" IS NULL", ""},
	"IsNotNull": {" IS NOT NULL", ""},
}

// wrapped are the operators that are FUNCTIONS in MySQL. They take the
// identifier as an argument rather than following it, so Frag has to build
// `JSON_CONTAINS(`col`, ?)` and not “ `col` JSON_CONTAINS ? “.
var wrapped = map[string]struct{ open, close string }{
	"JSONContains":    {"JSON_CONTAINS(", ", " + Placeholder + ")"},
	"JSONContainedBy": {"JSON_CONTAINS(" + Placeholder + ", ", ")"},
}

// Frag lowers one operator applied to one already-quoted identifier. ok is
// false when this back end has no lowering for the operator — which is a
// generation error naming the operator and the dialect, never a silent drop.
func Frag(op, ident string) (a, b string, ok bool) {
	if w, isWrapped := wrapped[op]; isWrapped {
		return w.open + ident, w.close, true
	}
	f, ok := frags[op]
	if !ok {
		return "", "", false
	}
	return ident + f.a, f.b, true
}

// InFrag lowers list membership, which is the operator that could have sunk M9.
//
// PostgreSQL lowers `In` to `= ANY($1)`: ONE placeholder for a whole list, so
// the statement text does not depend on how many values the caller passed.
// MySQL's `IN (?,?,?)` has value-dependent arity, which makes the shape key a
// function of REQUEST DATA rather than of the program — storm being an
// interpreter with extra steps, which is the thing it exists not to be.
//
// ADR-0010 settled it on JSON_TABLE: one bound JSON document, statement text
// independent of list length, and an index lookup rather than a scan.
//
// colType is why this is not in the operator table. The JSON_TABLE column must
// be declared with the SAME type as the column being matched — `COLUMNS (v
// BIGINT PATH '$')` against a BIGINT key. Declared as JSON it compares a JSON
// scalar against a native value, which is both wrong and unindexable; the type
// is what keeps `Index lookup` in the plan.
func InFrag(ident, colType string, negate bool) (a, b string) {
	op := " IN ("
	if negate {
		op = " NOT IN ("
	}
	return ident + op +
		"SELECT `v` FROM JSON_TABLE(" + Placeholder +
		", '$[*]' COLUMNS (`v` " + colType + " PATH '$')) AS `_storm_in`)", ""
}

// Supported reports whether this back end has a lowering for an operator, for
// codegen to check before it emits a fragment table with a hole in it.
func Supported(op string) bool {
	switch op {
	case "In", "NotIn":
		return true
	}
	_, wrappedOp := wrapped[op]
	_, plain := frags[op]
	return wrappedOp || plain
}
