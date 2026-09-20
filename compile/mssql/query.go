// Package mssql lowers query structure to SQL Server text.
//
// It is the third implementation of the seam's query side, and the first one
// that is not a variation on the other two. compile/mysql's header lists three
// differences from PostgreSQL, all of them spellings. SQL Server's differences
// are positional and grammatical, and four of them could not be expressed
// through the seam as M9 left it:
//
//   - Parameters are NAMED. `@p1`, and the name is what the TDS parameter
//     declaration binds against, so a reused ordinal binds once (ADR-0010's
//     carrier grew a Prefix for this — `@1` is not a terse `@p1`, it is a
//     syntax error, because a T-SQL identifier may not start with a digit).
//   - There is no row constructor. `(a, b) > (@p1, @p2)` does not parse, and
//     keyset pagination is how storm pages, so the comparison expands into the
//     OR-chain it means. Legal here precisely because the names above are
//     reusable.
//   - The row cap is part of ORDER BY. `OFFSET n ROWS FETCH NEXT m ROWS ONLY`
//     is a clause OF the ordering, so an unordered capped read is a syntax
//     error rather than an arbitrary order — hence OrderFallback. And its
//     operands are the reverse of `LIMIT n OFFSET m`, so the paging arguments
//     bind in the other order.
//   - OUTPUT is positional. It sits between the assignments and the predicate,
//     where RETURNING goes last on both other targets.
//
// What is the same is worth saying too, because it is what makes the milestone
// smaller than M9: SQL Server HAS the returning clause, lateral joins
// (CROSS APPLY), recursive CTEs, window functions, and — unlike MySQL —
// filtered indexes, so a soft-delete table's live-scoped unique ports here.
//
// Output is byte-deterministic: same input, same bytes, always.
package mssql

import "strings"

// Ident quotes an identifier.
//
// Brackets rather than double quotes. SQL Server accepts `"name"` only under
// QUOTED_IDENTIFIER ON, which is a per-connection setting a library may not
// assume about someone else's server — the same reasoning that makes MySQL's
// identifiers backticked here. A `]` inside a name is doubled.
func Ident(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }

// Placeholder marks where a bound parameter goes.
//
// The bare sigil: the splicer appends the prefix and the ordinal, so this is
// `@` at generate time and `@p1` in the statement. See runtime.MSSQLPlaceholder.
const Placeholder = "@"

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

// ExistsPrefix projects nothing and caps at one row.
//
// The cap is in the PREFIX here, not the suffix, because SQL Server's other cap
// — OFFSET/FETCH — requires an ORDER BY, and an existence probe has none to
// give. TOP has no such requirement, which is exactly what it is for.
func ExistsPrefix(table string) string { return "SELECT TOP 1 1 FROM " + Ident(table) }

// ExistsSuffix caps an existence probe — nothing to add; see ExistsPrefix.
func ExistsSuffix() string { return "" }

// LimitOffsetSuffix is the tail of a paged read.
//
// OFFSET is not optional. `FETCH NEXT n ROWS ONLY` without a preceding OFFSET
// is a syntax error, so the uncapped-offset form spells it `OFFSET 0 ROWS` as a
// literal rather than a bound zero — one fewer parameter, and the plan the
// server caches is the same one every time.
//
// The operands are reversed against `LIMIT n OFFSET m`: offset first. codegen
// binds the paging arguments in this order for this back end, which is what
// PagingOffsetFirst says.
func LimitOffsetSuffix(withOffset bool) string {
	if withOffset {
		return " OFFSET " + Placeholder + " ROWS FETCH NEXT " + Placeholder + " ROWS ONLY"
	}
	return " OFFSET 0 ROWS FETCH NEXT " + Placeholder + " ROWS ONLY"
}

// OrderFallback is the ordering a capped read falls back to when the caller
// asked for none.
//
// `(SELECT NULL)` is a constant the optimizer discards; it exists to satisfy
// the grammar, because OFFSET/FETCH is defined as a clause of ORDER BY. The
// alternative — ordering by the primary key — would be a sort the caller did
// not ask for and did not want.
const OrderFallback = " ORDER BY (SELECT NULL)"

// DefaultOrderBy is the ordering used when the caller names none: the primary
// key, because a read without ORDER BY has no defined order and paging one is a
// bug waiting for a plan change.
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

// OrderLead and OrderSep punctuate an ORDER BY built at query time.
const (
	OrderLead = " ORDER BY "
	OrderSep  = ", "
)

// Order directions, in the order runtime numbers them. The numbering is the
// token stream's, not a back end's to choose.
const (
	dirAsc = iota
	dirDesc
	dirAscNullsFirst
	dirDescNullsLast
)

// NDirections is how many orderings OrderTerm distinguishes.
const NDirections = 4

// OrderTerm lowers one ORDER BY term.
//
// SQL Server has no NULLS FIRST / NULLS LAST — it was added in no released
// version, and the 2019 level this targets has neither. NULL sorts as the
// LOWEST value, so ascending already puts nulls first and descending puts them
// last, which is the same arrangement MySQL has and the opposite of
// PostgreSQL's default.
//
// The two placements that already hold are therefore spelled plainly. Asking
// for the other one would need `CASE WHEN x IS NULL THEN 1 ELSE 0 END` as a
// leading key, which is an expression, so a plain index no longer satisfies the
// ordering and the server sorts. storm does not generate that form: a model
// that needs it on this target is asking for a sort, and `storm lint` is where
// that belongs.
func OrderTerm(dir int, ident string) string {
	switch dir {
	case dirDesc, dirDescNullsLast:
		return ident + " DESC"
	default:
		return ident
	}
}

// Row comparison — what keyset pagination filters with.
//
// SQL Server has NO row constructor: `(a, b) > (@p1, @p2)` is a syntax error,
// not a slower plan. compile/mysql's note on the same constants said "SQL
// Server does not, and M10 will have to expand it there", and this is that.
// The expansion lives in runtime.expandRowCmp, because it is the SPLICER that
// knows the ordinals; RowCmpExpand is how this package asks for it.
//
// The punctuation below is still supplied and still correct — it is what the
// expansion would use if this back end grew the constructor — but nothing reads
// it while RowCmpExpand is set.
const (
	TupleOpen  = "("
	TupleSep   = ", "
	TupleClose = ")"
)

// RowCmpExpand says the splicer must write the OR-chain rather than the tuple.
const RowCmpExpand = true

// PagingOffsetFirst says the paging arguments bind offset before limit,
// because OFFSET/FETCH names them in that order.
const PagingOffsetFirst = true

const (
	cmpGt = iota
	cmpLt
)

// RowCmpOp lowers a row-comparison operator. Strict inequality only, for the
// same reason as everywhere else: >= returns the row you just showed.
func RowCmpOp(op int) string {
	if op == cmpLt {
		return " < "
	}
	return " > "
}

// Section punctuation, identical to PostgreSQL's because SQL's is.
const (
	SetLead   = ""
	SetSep    = ", "
	WhereLead = " WHERE "
	WhereSep  = " AND "
)

// UpdatePrefix introduces the SET list.
func UpdatePrefix(table string) string { return "UPDATE " + Ident(table) + " SET " }

// DeletePrefix introduces a delete.
func DeletePrefix(table string) string { return "DELETE FROM " + Ident(table) }

// SetFrag assigns one column from a bound value.
func SetFrag(col string) (a, b string) { return Ident(col) + " = " + Placeholder, "" }

// BumpFrag increments a version column from its own value.
func BumpFrag(col string) (a, b string) { return Ident(col) + " = " + Ident(col) + " + 1", "" }

// NowFrag assigns a column the database's clock rather than the caller's.
//
// SYSDATETIMEOFFSET(), not GETDATE(): GETDATE() is a `datetime` in the server's
// LOCAL zone at 3.33 ms resolution — wrong type, wrong zone and coarse enough
// that two rows stamped in one statement compare equal. SYSDATETIMEOFFSET()
// carries the offset and 100 ns, which is what a timestamptz column means.
//
// Like MySQL's CURRENT_TIMESTAMP and unlike PostgreSQL's now(), this is read at
// STATEMENT time, so two statements in one transaction stamp different
// instants.
func NowFrag(col string) (a, b string) { return Ident(col) + " = SYSDATETIMEOFFSET()", "" }

// InsertPrefix introduces a masked insert.
func InsertPrefix(table string) string { return "INSERT INTO " + Ident(table) }

// Correlated semi-joins — "a related row exists".
//
// Standard SQL in shape, so this is PostgreSQL's lowering with this back end's
// identifier quoting. The inner table is ALWAYS aliased, for the same reason it
// is there: a self-referential relation correlates a table with itself, and
// without the alias the inner reference captures the outer one and the
// predicate silently means something else.
const existsAlias = "_storm_e"

// ExistsFrag lowers "a related row exists". childLive is the child's
// soft-delete predicate under the inner alias, or "" — "this parent has a
// related row" must not be satisfied by a row the child's own package would
// refuse to return.
func ExistsFrag(childTable, childFK, parentTable, parentPK string, childLive Live) string {
	return "EXISTS (SELECT 1 FROM " + Ident(childTable) + " AS " + Ident(existsAlias) +
		" WHERE " + Ident(existsAlias) + "." + Ident(childFK) +
		" = " + Ident(parentTable) + "." + Ident(parentPK) +
		aliasLive(childLive) + ")"
}

// NotExistsFrag lowers "no related row exists".
func NotExistsFrag(childTable, childFK, parentTable, parentPK string, childLive Live) string {
	return "NOT " + ExistsFrag(childTable, childFK, parentTable, parentPK, childLive)
}

// ExistsOpen is ExistsFrag without its closing paren: the splicer appends the
// wrapped child predicates and closes. Split here rather than string-surgered
// in codegen, so the two forms cannot drift.
func ExistsOpen(childTable, childFK, parentTable, parentPK string, childLive Live) string {
	f := ExistsFrag(childTable, childFK, parentTable, parentPK, childLive)
	return f[:len(f)-1]
}

// NotExistsOpen is ExistsOpen negated.
func NotExistsOpen(childTable, childFK, parentTable, parentPK string, childLive Live) string {
	return "NOT " + ExistsOpen(childTable, childFK, parentTable, parentPK, childLive)
}

// aliasLive re-qualifies a child predicate to the exists alias.
func aliasLive(l Live) string {
	if l.Empty() {
		return ""
	}
	return " AND " + LiveFor(existsAlias, liveCol(l))
}

// Live is this back end's soft-delete predicate, carried as a distinct type for
// the reason compile/pgsql's is: a read builder that takes one cannot forget it
// by receiving "" out of habit.
type Live string

// Empty reports whether there is nothing to add.
func (l Live) Empty() bool { return l == "" }

// liveCol recovers the column from a predicate LiveFor built.
func liveCol(l Live) string {
	s := string(l)
	i := strings.Index(s, " IS NULL")
	if i < 0 {
		return ""
	}
	s = s[:i]
	if j := strings.LastIndex(s, "."); j >= 0 {
		s = s[j+1:]
	}
	return strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
}
