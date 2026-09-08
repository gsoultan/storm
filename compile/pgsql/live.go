package pgsql

import "strings"

// Live is the predicate that keeps soft-deleted rows out of a read, or "" for a
// table that deletes rows for real.
//
// It is a distinct type, and every read builder that names a table takes one,
// because the failure this feature exists to prevent is a read that FORGETS the
// predicate. A `string` parameter can be forgotten by passing "" out of habit;
// a named type forces the caller to have thought about it once, and forces the
// COMPILER to point at any new read builder that has not.
//
// The value is a whole predicate, not a column, because where it goes differs:
// a base read splices it as a declared WHERE, a union puts it in each branch, a
// join qualifies it by alias and hangs it on the ON clause of an outer join
// rather than the WHERE — see LiveFor.
type Live string

// LiveFor builds the predicate for a table, optionally qualified by the alias it
// is joined under. col is the soft-delete column, or "" for no soft delete.
//
// The alias matters and is the whole reason this is not a constant: in
// `orders JOIN users AS u`, a bare `deleted_at IS NULL` is ambiguous when both
// tables have the column, and simply wrong when only the other one does.
func LiveFor(alias, col string) Live {
	if col == "" {
		return ""
	}
	if alias == "" {
		return Live(Ident(col) + " IS NULL")
	}
	return Live(Ident(alias) + "." + Ident(col) + " IS NULL")
}

// Empty reports whether there is nothing to add.
func (l Live) Empty() bool { return l == "" }

// And returns `where AND live`, or whichever of the two exists.
func (l Live) And(where string) string {
	switch {
	case l == "":
		return where
	case where == "":
		return string(l)
	default:
		return "(" + where + ") AND " + string(l)
	}
}

// Clause renders " WHERE <pred>" for a statement that has no WHERE of its own,
// and "" when there is nothing to add.
func (l Live) Clause() string {
	if l == "" {
		return ""
	}
	return " WHERE " + string(l)
}

// AndInto appends the predicate to a WHERE clause already being written.
func (l Live) AndInto(b *strings.Builder, hasWhere bool) {
	if l == "" {
		return
	}
	if hasWhere {
		b.WriteString(" AND ")
	} else {
		b.WriteString(" WHERE ")
	}
	b.WriteString(string(l))
}
