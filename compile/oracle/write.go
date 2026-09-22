package oracle

import (
	"errors"
	"strings"
)

// The write path.
//
// THERE IS NO RETURNING HERE, and that is a decision rather than an absence.
//
// Oracle HAS a returning clause — `RETURNING col INTO :n` — and storm cannot
// use it. The `INTO` binds OUTPUT parameters, which means the client has to
// declare a destination buffer per returned column before the statement runs.
// runtime.Executor carries `Query`, `Exec`, `CopyFrom` and `Batch`, and none of
// them has anywhere to put an out-parameter (ADR-0005 is about exactly this:
// the port's width is a decision, not an accident). Widening it for one target
// would put a concept in every back end's signature that only this one uses.
//
// So the answer is MySQL's, reached from a different direction: keys are
// generated CLIENT-SIDE, and an insert returns nothing. The unit of work needs
// ids before the rows exist anyway, which is why this costs less than it reads.
//
// What it does cost is stated plainly: a column whose value the DATABASE
// chooses — an IDENTITY, a DEFAULT SYSTIMESTAMP — is not readable back from
// the insert that wrote it. The generated package does not pretend otherwise;
// InsertStmt refuses a non-empty returning list rather than dropping it.

// ErrNoReturning is what a request for a returned column gets.
var ErrNoReturning = errors.New(
	"oracle: this target has no usable RETURNING clause — Oracle's binds OUTPUT " +
		"parameters (RETURNING ... INTO), which runtime.Executor does not carry, so storm " +
		"generates keys client-side instead; a value the database chooses cannot be read " +
		"back from the statement that wrote it")

// InsertStmt renders a whole insert.
func InsertStmt(table string, cols []string, returning []string) (string, error) {
	if len(returning) > 0 {
		return "", ErrNoReturning
	}
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(Ident(table))
	b.WriteString(" (")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(Ident(c))
	}
	b.WriteString(") VALUES (")
	for i := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		// The bare sigil; the splicer numbers it. Unlike MySQL's `?`, position
		// alone does not bind here — `:1` is a NAME that happens to look like
		// an ordinal, and a repeated one binds once.
		b.WriteString(Placeholder)
	}
	b.WriteString(")")
	return b.String(), nil
}

// InsertPrefix introduces a multi-row insert.
func InsertPrefix(table string) string { return "INSERT INTO " + Ident(table) }

// InsertParts is the punctuation a single-row insert is spliced from.
func InsertParts() (open, sep, mid, close string) {
	return " (", ", ", ") VALUES (", ")"
}

// ReturningClause is empty for every column list. See the note at the top of
// this file: the clause exists in the dialect and not in the port.
func ReturningClause([]string) string { return "" }

// DeletePrefix introduces a delete.
func DeletePrefix(table string) string { return "DELETE FROM " + Ident(table) }

// SetFrag assigns a column a bound value.
func SetFrag(col string) (a, b string) { return Ident(col) + " = " + Placeholder, "" }

// BumpFrag increments a version column, which is how optimistic concurrency is
// spelled: the UPDATE's predicate carries the version the caller read, and this
// moves it on so a second writer's predicate matches nothing.
func BumpFrag(col string) (a, b string) { return Ident(col) + " = " + Ident(col) + " + 1", "" }

// SoftDeleteWhere is the predicate that hides deleted rows.
func SoftDeleteWhere(col string) string { return Ident(col) + " IS NULL" }

// SoftDeleteSet stamps the deletion time.
func SoftDeleteSet(table, col string) string {
	return UpdatePrefix(table) + Ident(col) + " = SYSTIMESTAMP"
}

// RestoreSet clears it.
func RestoreSet(table, col string) string {
	return UpdatePrefix(table) + Ident(col) + " = NULL"
}

// LiveFor qualifies the soft-delete predicate to an alias, or to nothing.
func LiveFor(alias, col string) string {
	if col == "" {
		return ""
	}
	if alias == "" {
		return SoftDeleteWhere(col)
	}
	return Ident(alias) + "." + SoftDeleteWhere(col)
}

// Live is a soft-delete predicate under some alias, or empty.
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
	return strings.Trim(s, `"`)
}

// existsAlias is the inner name a semi-join gives the child table. QUOTED
// wherever it is written, because an unquoted Oracle identifier may not begin
// with an underscore — ORA-00911, which no other target storm has cares about.
const existsAlias = "_storm_x"

// ExistsFrag lowers "a related row exists". childLive is the child's
// soft-delete predicate under the inner alias, or "" — "this parent has a
// related row" must not be satisfied by a row the child's own package would
// refuse to return.
//
// No AS before the alias: Oracle's table alias is juxtaposition, and `FROM t AS
// x` is ORA-00933. Every other target storm has accepts the keyword.
func ExistsFrag(childTable, childFK, parentTable, parentPK string, childLive Live) string {
	return "EXISTS (SELECT 1 FROM " + Ident(childTable) + " " + Ident(existsAlias) +
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
