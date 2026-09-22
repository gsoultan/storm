// Package oracle lowers query structure to Oracle SQL text.
//
// It is the fourth implementation of the seam's query side. compile/mysql's
// differences from PostgreSQL were spellings; compile/mssql's were positional
// and grammatical. Oracle's are a third kind: most of the GRAMMAR matches
// SQL Server's — OFFSET/FETCH, MERGE, CROSS APPLY, no row constructor — and
// the divergences that matter are about what the engine will REFUSE to
// combine, and about a semantic nothing else has.
//
//   - Parameters are `:1`, `:2`. Numeric bind variables, and a repeated
//     ordinal binds once, so ADR-0010's carrier needs no Prefix here (unlike
//     T-SQL, where `@1` is a syntax error because an identifier may not start
//     with a digit).
//   - There is no row constructor for INEQUALITY. `(a, b) > (:1, :2)` does not
//     parse — `IN` takes one and `>` does not — so keyset pagination expands
//     into the OR-chain it means, exactly as it does on SQL Server.
//   - A ROW CAP AND A ROW LOCK CANNOT BE COMBINED. `FETCH FIRST n ROWS ONLY
//     … FOR UPDATE` is ORA-02014, because FETCH FIRST is implemented as an
//     inline view and Oracle will not lock through one. That is the work-queue
//     shape — one statement on all three other targets — and it is REFUSED by
//     name here rather than silently reshaped. See LockRefusedCapped.
//   - There is no shared ROW lock at all. FOR UPDATE has no FOR SHARE
//     counterpart; `LOCK TABLE … IN SHARE MODE` is a table lock and a
//     different promise. Three of the seven lock modes are refused.
//   - The empty string is NULL. Not lowerable and not lowered: it is made
//     CONSISTENT by compile/oraddl refusing the nullable text column that is
//     the only place a stored `”` could have existed.
//
// What is the SAME is worth saying, because it is what keeps the milestone at
// six weeks: Oracle has the returning clause (RETURNING … INTO), lateral joins
// (CROSS APPLY), recursive CTEs, window functions, GROUPING SETS, and — unlike
// MySQL — an exact partial UNIQUE, so a soft-delete table's live-scoped unique
// ports here. Every one of those was measured before this package was written;
// see internal/oraclespike/README.md.
//
// Output is byte-deterministic: same input, same bytes, always.
package oracle

import "strings"

// Ident quotes an identifier with double quotes, which is Oracle's spelling —
// and which also PRESERVES CASE. An unquoted name folds to UPPER here, where
// PostgreSQL folds it to lower, so quoting is what keeps a storm model's
// lowercase names resolvable at all.
func Ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Placeholder is the sigil the splicer numbers. `:` and the ordinal, with no
// prefix: `:1` is a legal bind variable here, where T-SQL's `@1` is not.
const Placeholder = ":"

// SelectPrefix is everything before the WHERE clause of a row read.
func SelectPrefix(table string, cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = Ident(c)
	}
	return "SELECT " + strings.Join(q, ", ") + " FROM " + Ident(table)
}

// CountPrefix counts rows.
//
// Plain count(), unlike SQL Server's count_big: Oracle's count() returns a
// NUMBER, which has no 32-bit ceiling to overflow and no width mismatch with
// the int64 a Count() decodes into.
func CountPrefix(table string) string { return "SELECT count(*) FROM " + Ident(table) }

// ExistsPrefix projects nothing; the cap is in the suffix.
//
// Unlike SQL Server, where the cap had to move into the prefix because
// OFFSET/FETCH requires an ORDER BY and a probe has none to give: Oracle's
// FETCH FIRST has no such requirement, so the ordinary suffix works.
func ExistsPrefix(table string) string { return "SELECT 1 FROM " + Ident(table) }

// ExistsSuffix caps an existence probe at one row.
func ExistsSuffix() string { return " FETCH FIRST 1 ROWS ONLY" }

// LimitOffsetSuffix is the tail of a paged read.
//
// The operands are reversed against `LIMIT n OFFSET m`: offset first, which is
// what PagingOffsetFirst says. Unlike SQL Server, OFFSET is optional here —
// `FETCH FIRST n ROWS ONLY` on its own parses — so the uncapped-offset form
// omits it rather than spelling a literal zero.
func LimitOffsetSuffix(withOffset bool) string {
	if withOffset {
		return " OFFSET " + Placeholder + " ROWS FETCH NEXT " + Placeholder + " ROWS ONLY"
	}
	return " FETCH FIRST " + Placeholder + " ROWS ONLY"
}

// OrderFallback is empty: OFFSET/FETCH is not a clause of ORDER BY here, so an
// unordered capped read PARSES.
//
// That it parses does not make it a good idea, and storm still emits the
// primary key by default — see DefaultOrderBy. The difference from SQL Server
// is only that nothing has to be invented to satisfy the grammar.
const OrderFallback = ""

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

// Ordering directions, matching the seam's.
const (
	dirAsc = iota
	dirDesc
	dirAscNullsFirst
	dirDescNullsLast
)

// NDirections is how many ORDER BY terms a generated package emits per key.
const NDirections = 4

// OrderTerm lowers one ORDER BY term.
//
// Oracle HAS NULLS FIRST and NULLS LAST, which neither MySQL nor SQL Server
// does — so unlike those two, the two non-default placements are spelled
// rather than refused or silently dropped. The defaults are still omitted:
// Oracle sorts NULLs LAST ascending and FIRST descending, which is
// PostgreSQL's arrangement exactly, so the two spellings that already hold
// cost nothing to leave out and keep an index able to satisfy the ordering.
func OrderTerm(dir int, ident string) string {
	switch dir {
	case dirDesc:
		return ident + " DESC"
	case dirAscNullsFirst:
		return ident + " NULLS FIRST"
	case dirDescNullsLast:
		return ident + " DESC NULLS LAST"
	default:
		return ident
	}
}

// OrderLead and OrderSep punctuate the ORDER BY clause.
const (
	OrderLead = " ORDER BY "
	OrderSep  = ", "
)

// Row comparison — what keyset pagination filters with.
//
// Oracle has NO row constructor for inequality. `(a, b) > (:1, :2)` is
// ORA-00920, not a slower plan — the constructor exists for IN and for nothing
// else. So the comparison expands into the OR-chain it means, in
// runtime.expandRowCmp, because it is the SPLICER that knows the ordinals.
// RowCmpExpand is how this package asks for it.
//
// The punctuation below is still supplied and still correct — it is what the
// expansion would use if this back end grew the constructor — but nothing
// reads it while RowCmpExpand is set.
const (
	TupleOpen  = "("
	TupleSep   = ", "
	TupleClose = ")"
)

// RowCmpExpand asks the splicer to expand a row comparison into an OR-chain.
const RowCmpExpand = true

// PagingOffsetFirst says the paging arguments bind offset-then-limit.
const PagingOffsetFirst = true

// Comparison directions for a keyset filter.
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

// NowFrag assigns a column the database's clock rather than the caller's.
//
// SYSTIMESTAMP, not CURRENT_TIMESTAMP: CURRENT_TIMESTAMP is read in the
// SESSION's time zone, which is a client setting, while SYSTIMESTAMP is the
// server's own clock with its own offset. A column whose value depends on who
// connected is not a timestamp anybody can reason about.
//
// Like MySQL's and SQL Server's and unlike PostgreSQL's now(), this is read at
// STATEMENT time, so two statements in one transaction stamp different
// instants.
func NowFrag(col string) (a, b string) { return Ident(col) + " = SYSTIMESTAMP", "" }
