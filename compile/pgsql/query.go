// Package pgsql lowers query structure to PostgreSQL text.
//
// It is the *only* place a SELECT keyword, an identifier quote or a `$`
// placeholder is written for the read path. codegen/ asks for strings and
// emits them; it never spells SQL itself. scripts/check/boundaries.sh enforces
// that, so the seam cannot rot back shut while Postgres is the only back end
// exercising it (risk R9).
//
// This package is deliberately *not* an implementation of a Dialect interface.
// There is one back end today, and the right abstraction over two back ends is
// not knowable from one. M9 (MySQL) generalises it, with two implementations
// in hand to generalise from. Until then this is relocation, not architecture.
//
// Output is byte-deterministic: same input, same bytes, always.
package pgsql

import "strings"

// Ident quotes an identifier. Postgres folds unquoted identifiers to lower
// case, so everything is quoted: a column named "Order" and a column named
// "order" are then different columns, which is what the model said.
//
// A raorm identifier comes from a Go field name or an explicit .Named(), never
// from a runtime value, so there is nothing here to escape — but a quote in an
// identifier would still break the statement, so it is doubled.
func Ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Placeholder marks where a bound parameter goes inside a fragment. The
// runtime appends the ordinal; see runtime.SpliceTree.
const Placeholder = "$"

// SelectPrefix is everything before the WHERE clause of a row read.
func SelectPrefix(table string, cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = Ident(c)
	}
	return "SELECT " + strings.Join(q, ", ") + " FROM " + Ident(table)
}

// CountPrefix is everything before the WHERE clause of a count.
func CountPrefix(table string) string { return "SELECT count(*) FROM " + Ident(table) }

// OrderSuffix is everything after the WHERE clause: the ordering, then LIMIT
// with a placeholder the runtime numbers.
func OrderSuffix(orderBy string) string {
	return " ORDER BY " + orderBy + " LIMIT " + Placeholder
}

// ExistsSuffix caps an existence probe. Ordering an EXISTS is wasted work.
func ExistsSuffix() string { return " LIMIT 1" }

// DefaultOrderBy is the ordering used when the caller names none. A read
// without ORDER BY has no defined order, so paging one is a bug waiting for a
// plan change; the primary key is the cheapest total order available.
func DefaultOrderBy(primaryKey []string, fallback string) string {
	if len(primaryKey) == 0 {
		return Ident(fallback)
	}
	parts := make([]string, len(primaryKey))
	for i, k := range primaryKey {
		parts[i] = Ident(k)
	}
	return strings.Join(parts, ", ")
}

// frags is the SQL for each operator, split where its placeholder goes: A,
// then the ordinal the runtime appends, then B. An operator absent from this
// table is one Postgres cannot express, and asking for it is a generation
// error rather than a silently dropped predicate.
//
// `In` lowers to `= ANY($1)`, which binds a whole list to ONE placeholder.
// That is what makes the relation batch loader a fixed two round trips
// regardless of parent count. It is also the first thing M9 will have to
// rethink: MySQL's `IN (?, ?, ?)` has an arity that depends on the value, so
// the shape key would stop being value-independent. Do not assume the split
// below carries over.
var frags = map[string]struct{ a, b string }{
	"Eq":        {" = " + Placeholder, ""},
	"NotEq":     {" <> " + Placeholder, ""},
	"Gt":        {" > " + Placeholder, ""},
	"Gte":       {" >= " + Placeholder, ""},
	"Lt":        {" < " + Placeholder, ""},
	"Lte":       {" <= " + Placeholder, ""},
	"Like":      {" LIKE " + Placeholder, ""},
	"In":        {" = ANY(" + Placeholder, ")"},
	"IsNull":    {" IS NULL", ""},
	"IsNotNull": {" IS NOT NULL", ""},
}

// Frag lowers one operator applied to one already-quoted identifier. ok is
// false when this back end has no lowering for the operator.
func Frag(op, ident string) (a, b string, ok bool) {
	f, ok := frags[op]
	if !ok {
		return "", "", false
	}
	return ident + f.a, f.b, true
}

// Ordering.
//
// ORDER BY is chosen at query time, not at generate time, so its text is built
// from a table the back end fills here rather than baked into a constant. Two
// queries differing only in ordering are different statements.

// OrderLead and OrderSep punctuate the clause.
const (
	OrderLead = " ORDER BY "
	OrderSep  = ", "
)

// Directions, in the order runtime's constants number them.
const (
	dirAsc = iota
	dirDesc
	dirAscNullsFirst
	dirDescNullsLast
)

// OrderTerm lowers one ordering term.
//
// Defaults are not spelled out. ASC is Postgres's default direction, and NULLs
// go last for ASC and first for DESC, so writing either again changes nothing
// and makes the statement harder to read against a hand-written one. The other
// two NULL combinations ARE spelled out: they are what a keyset paginator
// needs, and leaving them implicit would silently change the order.
//
// This is not only cosmetic. bench asserts that generated SQL is byte-identical
// to the hand-written M0 spike, so that the two are compared on one query plan
// and not two — and a stray "ASC" is enough to break that.
func OrderTerm(dir int, ident string) string {
	switch dir {
	case dirDesc:
		return ident + " DESC"
	case dirAscNullsFirst:
		return ident + " ASC NULLS FIRST"
	case dirDescNullsLast:
		return ident + " DESC NULLS LAST"
	default:
		return ident
	}
}

// NDirections is how many orderings OrderTerm distinguishes.
const NDirections = 4

// LimitOffsetSuffix is the tail of a paged read. Both take placeholders the
// runtime numbers.
//
// OFFSET is generated because callers expect it, not because it is a good idea:
// the database still walks and discards every skipped row, so page 5,000 costs
// 5,000 pages of work. Keyset pagination is the answer, and `raorm lint` is
// where a large constant offset should get flagged.
func LimitOffsetSuffix(withOffset bool) string {
	s := " LIMIT " + Placeholder
	if withOffset {
		s += " OFFSET " + Placeholder
	}
	return s
}

// Row comparison — what keyset pagination filters with.
//
// `(a, b) > ($1, $2)` rather than the OR-expansion
// `a > $1 OR (a = $1 AND b > $2)` that hand-written pagination usually reaches
// for. The two mean the same thing; only the row comparison lets Postgres walk
// a multi-column index once instead of planning a disjunction. It is also the
// first thing M9 will have to look at again — MySQL supports row comparison,
// SQL Server does not, and there the expansion is the only option.
const (
	TupleOpen  = "("
	TupleSep   = ", "
	TupleClose = ")"
)

// Row-comparison operators, in the order runtime numbers them.
const (
	cmpGt = iota
	cmpLt
)

// RowCmpOp lowers a row-comparison operator.
//
// Only strict inequality. A keyset paginator that used >= would return the row
// it just showed you, and one that used = has no ordering to follow.
func RowCmpOp(op int) string {
	if op == cmpLt {
		return " < "
	}
	return " > "
}
