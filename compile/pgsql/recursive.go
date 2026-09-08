package pgsql

import "strings"

// Recursive traversal of a self-reference: a whole subtree or ancestor chain in
// one query.
//
// Two things are mandatory here and neither is a default a caller can drop.
//
// A DEPTH BOUND. An unbounded recursive query against production data is an
// outage, not a slow query — it is the one shape where a missing WHERE turns
// into unbounded work rather than a large result. So depth is a bound
// parameter, not an option.
//
// A CYCLE GUARD. `parent_id` is a foreign key, and a foreign key does not stop
// A pointing at B pointing at A. Postgres will happily recurse forever on that;
// Postgres 14+ has CYCLE syntax but storm still targets 13, so the guard is an
// explicit path array. It costs one array append per row and it is the
// difference between a bad row of data and a hung connection.

// Recursion direction.
const (
	Descend = iota // children of the given roots
	Ascend         // ancestors of the given rows
)

// Recursive lowers a self-referential traversal.
//
// table is the self-referencing table, cols its projected columns, key its
// primary key and parent the column pointing at that key. Two placeholders: the
// root id array, then the maximum depth.
func Recursive(table string, cols []string, key, parent string, dir int, live Live) string {
	var b strings.Builder
	t := Ident(recursiveAlias)
	c := Ident(recursiveChild)

	b.WriteString("WITH RECURSIVE ")
	b.WriteString(t)
	b.WriteString(" AS (SELECT ")
	writeIdents(&b, cols)
	b.WriteString(", 1 AS ")
	b.WriteString(Ident(depthAlias))
	b.WriteString(", ARRAY[")
	b.WriteString(Ident(key))
	b.WriteString("] AS ")
	b.WriteString(Ident(pathAlias))
	b.WriteString(" FROM ")
	b.WriteString(Ident(table))
	b.WriteString(" WHERE ")
	b.WriteString(Ident(key))
	b.WriteString(" = ANY(")
	b.WriteString(Placeholder)
	b.WriteString("1)")
	// The anchor: a deleted row must not seed the traversal.
	live.AndInto(&b, true)
	b.WriteString(" UNION ALL SELECT ")
	for i, col := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(c)
		b.WriteString(".")
		b.WriteString(Ident(col))
	}
	b.WriteString(", ")
	b.WriteString(t)
	b.WriteString(".")
	b.WriteString(Ident(depthAlias))
	b.WriteString(" + 1, ")
	b.WriteString(t)
	b.WriteString(".")
	b.WriteString(Ident(pathAlias))
	b.WriteString(" || ")
	b.WriteString(c)
	b.WriteString(".")
	b.WriteString(Ident(key))
	b.WriteString(" FROM ")
	b.WriteString(Ident(table))
	b.WriteString(" ")
	b.WriteString(c)
	b.WriteString(" JOIN ")
	b.WriteString(t)
	b.WriteString(" ON ")

	// Descending follows children: a row whose parent is one we already have.
	// Ascending follows the parent link the other way.
	if dir == Ascend {
		b.WriteString(t + "." + Ident(parent) + " = " + c + "." + Ident(key))
	} else {
		b.WriteString(c + "." + Ident(parent) + " = " + t + "." + Ident(key))
	}

	b.WriteString(" WHERE ")
	b.WriteString(t)
	b.WriteString(".")
	b.WriteString(Ident(depthAlias))
	b.WriteString(" < ")
	b.WriteString(Placeholder)
	b.WriteString("2")
	// BOTH halves, and qualified to the child: the recursive term joins the
	// table to itself, so an unqualified predicate is ambiguous, and one that
	// guarded only the anchor would let a deleted row back in on the second
	// iteration — carrying its whole subtree with it.
	if !live.Empty() {
		b.WriteString(" AND ")
		b.WriteString(string(LiveFor(recursiveChild, liveCol(live))))
	}
	b.WriteString(" AND NOT ")
	b.WriteString(c)
	b.WriteString(".")
	b.WriteString(Ident(key))
	b.WriteString(" = ANY(")
	b.WriteString(t)
	b.WriteString(".")
	b.WriteString(Ident(pathAlias))
	b.WriteString(")) SELECT ")
	writeIdents(&b, cols)
	b.WriteString(" FROM ")
	b.WriteString(t)
	return b.String()
}

const (
	recursiveAlias = "_storm_r"
	recursiveChild = "_storm_rc"
	depthAlias     = "_storm_d"
	pathAlias      = "_storm_path"
)

// liveCol recovers the column from a predicate built by LiveFor. The recursive
// term needs the same column under a different alias, and passing the whole
// predicate is what every other caller wants — so this unpicks it here rather
// than widening every signature for the one shape that needs both.
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
