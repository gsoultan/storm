package oracle

// Operator lowering for Oracle.
//
// Three groups differ from PostgreSQL's table in kind rather than in spelling:
//
//   - The array, range and network operators have no Oracle equivalent.
//     compile/oraddl already refuses those COLUMN types, and their absence
//     here is the same refusal on the read path, so the two cannot disagree
//     about what the dialect supports.
//   - `In` is ADR-0010's problem again, with the same shape and a third
//     function. `IN (:1, :2, :3)` makes the statement text depend on how many
//     values the caller passed, which makes the shape key a function of request
//     data rather than of the program. JSON_TABLE takes ONE bound document and
//     unpacks it, so the text is fixed — the same answer MySQL gave, under the
//     same name.
//   - The JSON operators are the one place Oracle is RICHER than SQL Server:
//     JSON_EXISTS answers key presence directly, so HasAnyKey and HasAllKeys
//     are both expressible where SQL Server could manage only the first.
//
// AND ONE THING THAT IS NOT AN OPERATOR AT ALL. Oracle's empty string is NULL,
// so `Eq` against `''` matches nothing. That is not fixed here and cannot be:
// the value arrives at runtime. It is made CONSISTENT by compile/oraddl
// refusing the nullable text column that is the only place a stored `''` could
// have existed — see checkEmptyString there, and internal/oraclespike's
// measurement behind it.

type frag struct{ a, b string }

var frags = map[string]frag{
	"Eq":    {" = " + Placeholder, ""},
	"NotEq": {" <> " + Placeholder, ""},
	"Gt":    {" > " + Placeholder, ""},
	"Gte":   {" >= " + Placeholder, ""},
	"Lt":    {" < " + Placeholder, ""},
	"Lte":   {" <= " + Placeholder, ""},
	"Like":  {" LIKE " + Placeholder, ""},

	// Case-insensitive LIKE, spelled rather than left to the session.
	// Oracle's NLS_COMP/NLS_SORT can make plain comparisons case-insensitive,
	// and they are SESSION settings — so a model that is portable would stop
	// being portable depending on who set the client's environment. The
	// linguistic form is the one an index can be built on
	// (NLSSORT(col, 'NLS_SORT=BINARY_CI')).
	"ILike": {" COLLATE BINARY_CI LIKE " + Placeholder, ""},

	// In and NotIn are NOT here: their lowering needs the column's SQL type.
	// See InFrag.

	"IsNull":    {" IS NULL", ""},
	"IsNotNull": {" IS NOT NULL", ""},

	// Case-insensitive equality written as the expression a function-based
	// index can be built on. Must exist and must mean the same thing as
	// everywhere else, or a portable model stops being portable at the first
	// case-insensitive lookup.
	"EqLower": {") = LOWER(" + Placeholder, ")"},
}

// wrapped are the operators that are FUNCTIONS here. They take the identifier
// as an argument rather than following it.
// keyAlias is the alias the unpacked key list is given.
//
// QUOTED, and that is not decoration. An unquoted Oracle identifier must begin
// with a LETTER, so `_storm_k` is ORA-00911 "invalid character" — where every
// other target storm has accepts a leading underscore happily. Every internal
// alias this package invents goes through Ident for that reason.
var keyAlias = Ident("_storm_k")

var wrapped = map[string]struct{ open, close string }{
	// PostgreSQL's `?|` — does this document have ANY of these top-level keys.
	// JSON_EXISTS with a path that names the keys answers it directly, and the
	// key list is ONE bound array unpacked by JSON_TABLE, so the statement's
	// shape does not depend on how many keys the caller passed.
	"HasAnyKey": {
		"EXISTS (SELECT 1 FROM JSON_TABLE(" + Placeholder +
			", '$[*]' COLUMNS (k VARCHAR2(4000) PATH '$')) " + keyAlias +
			" WHERE JSON_EXISTS(",
		", '$.' || " + keyAlias + ".k))",
	},
	// And `?&` — ALL of them. Expressible here because the bound list is
	// mentioned once and counted by the same subquery, which is exactly what
	// SQL Server could not do with a single-placeholder fragment.
	"HasAllKeys": {
		"(SELECT count(*) FROM JSON_TABLE(" + Placeholder +
			", '$[*]' COLUMNS (k VARCHAR2(4000) PATH '$')) " + keyAlias +
			" WHERE NOT JSON_EXISTS(",
		", '$.' || " + keyAlias + ".k)) = 0",
	},
}

// refused are the operators this back end has no honest lowering for, each with
// the reason a generation error will carry.
//
// JSON containment is the group, as it was on SQL Server and for a narrower
// reason. Oracle has JSON_EXISTS, JSON_VALUE, JSON_QUERY and JSON_TABLE, and no
// containment PREDICATE: there is no `@>` and nothing that asks whether one
// document contains another. Answering it would mean walking both with
// JSON_TABLE recursively, which is not a fragment and is not something storm
// may write on a caller's behalf without them knowing its cost.
var refused = map[string]string{
	"JSONContains":    "Oracle has no JSON containment predicate (no @>, no JSON_CONTAINS); its JSON support is JSON_EXISTS, JSON_VALUE, JSON_QUERY and JSON_TABLE",
	"JSONContainedBy": "Oracle has no JSON containment predicate (no @>, no JSON_CONTAINS); its JSON support is JSON_EXISTS, JSON_VALUE, JSON_QUERY and JSON_TABLE",
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
// JSON_TABLE with an explicit COLUMNS clause, for the reason compile/mysql's
// has one and compile/mssql's OPENJSON has a WITH: the unpacked column must
// carry the SAME type as the column it is matched against. Left to a default
// every value comes back as text, and comparing that to a NUMBER key is an
// implicit conversion on the COLUMN side of the predicate — which Oracle will
// do, and which makes the index unusable. The declared type is what keeps an
// index range scan in the plan.
//
// `'$'` as the path is the element itself, which is how a JSON array of scalars
// is unpacked.
func InFrag(ident, colType string, negate bool) (a, b string) {
	op := " IN ("
	if negate {
		op = " NOT IN ("
	}
	return ident + op +
		"SELECT v FROM JSON_TABLE(" + Placeholder +
		", '$[*]' COLUMNS (v " + colType + " PATH '$')))", ""
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
