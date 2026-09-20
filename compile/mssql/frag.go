package mssql

// Operator lowering for SQL Server.
//
// Three groups differ from PostgreSQL's table in kind rather than in spelling:
//
//   - The array, range and network operators have no SQL Server equivalent.
//     compile/msddl already refuses those COLUMN types, and their absence here
//     is the same refusal on the read path, so the two cannot disagree about
//     what the dialect supports.
//   - `In` is ADR-0010's problem again, and the answer has the same shape it
//     had on MySQL with a different function. `IN (@p1, @p2, @p3)` makes the
//     statement text depend on how many values the caller passed, which makes
//     the shape key a function of request data rather than of the program.
//     OPENJSON takes ONE bound document and unpacks it, so the text is fixed.
//   - The JSON containment operators are REFUSED rather than approximated. See
//     the note on refused, below.
type frag struct{ a, b string }

var frags = map[string]frag{
	"Eq":    {" = " + Placeholder, ""},
	"NotEq": {" <> " + Placeholder, ""},
	"Gt":    {" > " + Placeholder, ""},
	"Gte":   {" >= " + Placeholder, ""},
	"Lt":    {" < " + Placeholder, ""},
	"Lte":   {" <= " + Placeholder, ""},
	"Like":  {" LIKE " + Placeholder, ""},

	// Named explicitly rather than relying on the database's collation. A SQL
	// Server installed with a CI collation makes plain LIKE case-insensitive
	// already, and one installed with a CS collation does not — so a model that
	// is portable would stop being portable depending on who ran setup.
	"ILike": {" COLLATE Latin1_General_CI_AS LIKE " + Placeholder, ""},

	// In and NotIn are NOT here: their lowering needs the column's SQL type.
	// See InFrag.

	"IsNull":    {" IS NULL", ""},
	"IsNotNull": {" IS NOT NULL", ""},

	// Case-insensitive equality written as the expression an index can be built
	// on — SQL Server indexes a computed column, and LOWER(col) is the one to
	// compute. Redundant under a CI collation, like ILike above, and it must
	// exist and mean the same thing or a portable model stops being portable at
	// the first case-insensitive lookup.
	"EqLower": {") = LOWER(" + Placeholder, ")"},
}

// wrapped are the operators that are FUNCTIONS here. They take the identifier
// as an argument rather than following it.
var wrapped = map[string]struct{ open, close string }{
	// PostgreSQL's `?|` asks whether a document has ANY of a list of top-level
	// keys. SQL Server has no operator for it, but OPENJSON over the document
	// yields its keys as rows, and OPENJSON over the bound array yields the
	// wanted ones, so the question is a join between the two.
	//
	// One bound value, so the statement's shape does not depend on how many
	// keys the caller passed — the property that made JSON_TABLE the answer on
	// MySQL and `= ANY` the answer on PostgreSQL.
	"HasAnyKey": {
		"EXISTS (SELECT 1 FROM OPENJSON(",
		") AS [_storm_k] JOIN OPENJSON(" + Placeholder +
			") AS [_storm_v] ON [_storm_k].[key] = [_storm_v].[value])",
	},
}

// refused are the operators this back end has no honest lowering for, each with
// the reason a generation error will carry.
//
// JSON containment is the group. SQL Server through the 2019 level has
// JSON_VALUE, JSON_QUERY, ISJSON and OPENJSON, and no containment predicate at
// all: there is no JSON_CONTAINS and no `@>`. Answering "does this document
// contain that one" would mean walking both with OPENJSON recursively, which is
// not a fragment and is not something storm may write on a caller's behalf
// without them knowing its cost.
//
// HasAllKeys is refused for a narrower and more mechanical reason: the only
// formulation counts the bound array twice, and a Frag carries exactly one
// placeholder — its LAST byte is the sigil the splicer numbers. A second
// mention has nowhere to go.
var refused = map[string]string{
	"JSONContains":    "SQL Server has no JSON containment predicate (no JSON_CONTAINS, no @>); through the 2019 level its JSON support is JSON_VALUE, JSON_QUERY, ISJSON and OPENJSON",
	"JSONContainedBy": "SQL Server has no JSON containment predicate (no JSON_CONTAINS, no @>); through the 2019 level its JSON support is JSON_VALUE, JSON_QUERY, ISJSON and OPENJSON",
	"HasAllKeys":      "SQL Server can ask whether a document has ALL of a list of keys only by counting the bound list twice, and a compiled fragment carries one parameter",
}

// prefixes wrap the IDENTIFIER for operators whose left side is an expression
// rather than the bare column. Absent means no prefix.
var prefixes = map[string]string{
	"EqLower": "LOWER(",
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
	return prefixes[op] + ident + f.a, f.b, true
}

// InFrag lowers list membership.
//
// OPENJSON with an explicit WITH clause, for the reason compile/mysql's
// JSON_TABLE has one: the unpacked column must carry the SAME type as the
// column it is matched against. Left to OPENJSON's default schema every value
// comes back NVARCHAR(4000), and comparing that to a BIGINT key is an implicit
// conversion on the COLUMN side of the predicate — which SQL Server will do,
// and which makes the index unusable. The declared type is what keeps an index
// seek in the plan rather than a scan.
//
// `'$'` as the path is the element itself, which is how a JSON array of scalars
// is unpacked.
func InFrag(ident, colType string, negate bool) (a, b string) {
	op := " IN ("
	if negate {
		op = " NOT IN ("
	}
	return ident + op +
		"SELECT [v] FROM OPENJSON(" + Placeholder +
		") WITH ([v] " + colType + " '$'))", ""
}

// Refused returns why this back end has no lowering for an operator, or "".
func Refused(op string) string { return refused[op] }

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
