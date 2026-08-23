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
// Postgres 14+ has CYCLE syntax but raorm still targets 13, so the guard is an
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
func Recursive(table string, cols []string, key, parent string, dir int) string {
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
	b.WriteString("1) UNION ALL SELECT ")
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
	b.WriteString("2 AND NOT ")
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
	recursiveAlias = "_raorm_r"
	recursiveChild = "_raorm_rc"
	depthAlias     = "_raorm_d"
	pathAlias      = "_raorm_path"
)
