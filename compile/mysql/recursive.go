package mysql

import "strings"

// Recursive traversal of a self-reference: a whole subtree or ancestor chain in
// one query rather than one query per level.
//
// MySQL 8 has WITH RECURSIVE, so the shape carries over. What does not is the
// CYCLE GUARD. PostgreSQL accumulates the visited keys in an ARRAY — `ARRAY[k]`,
// `path || k`, `NOT k = ANY(path)` — and MySQL has no array type.
//
// The path is a HEX string here, joined by commas and tested with FIND_IN_SET.
// HEX rather than CAST(… AS CHAR) because the key may be BINARY(16) — a uuid,
// which is what storm.Model uses — and a raw binary cast can contain the comma
// that separates the list, which would make the guard read one visited key as
// two and stop guarding. HEX output is [0-9A-F] for every input type, so the
// separator cannot occur inside an element.
//
// Verified on 8.4.11: a subtree traverses to the right depths, and a 6↔7 cycle
// terminates at depth 2 rather than running to cte_max_recursion_depth.

// Traversal directions, numbered as the generated Query numbers them.
const (
	Descend = iota // children of the given roots
	Ascend         // ancestors of the given rows
)

// Recursive lowers a self-referential traversal.
//
// keyType is the SQL type of the key, needed because the roots arrive as a
// bound JSON document that JSON_TABLE unpacks — MySQL has no array parameter to
// compare against directly.
func Recursive(table string, cols []string, key, parent, keyType string, dir int, live Live) string {
	var b strings.Builder
	t := Ident(recursiveAlias)
	c := Ident(recursiveChild)
	hexKey := func(qualified string) string { return "HEX(" + qualified + ")" }

	b.WriteString("WITH RECURSIVE ")
	b.WriteString(t)
	b.WriteString(" AS (SELECT ")
	writeIdents(&b, cols)
	b.WriteString(", 1 AS ")
	b.WriteString(Ident(depthAlias))
	b.WriteString(", ")
	b.WriteString(hexKey(Ident(key)))
	b.WriteString(" AS ")
	b.WriteString(Ident(pathAlias))
	b.WriteString(" FROM ")
	b.WriteString(Ident(table))
	b.WriteString(" WHERE ")
	b.WriteString(Ident(key))
	b.WriteString(" IN (SELECT ")
	b.WriteString(keyValue(keyType, ""))
	b.WriteString(" FROM ")
	b.WriteString(keyRows(keyType))
	b.WriteString(")")
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
	b.WriteString(" + 1, CONCAT(")
	b.WriteString(t)
	b.WriteString(".")
	b.WriteString(Ident(pathAlias))
	b.WriteString(", ',', ")
	b.WriteString(hexKey(c + "." + Ident(key)))
	b.WriteString(") FROM ")
	b.WriteString(Ident(table))
	b.WriteString(" ")
	b.WriteString(c)
	b.WriteString(" JOIN ")
	b.WriteString(t)
	b.WriteString(" ON ")
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
	// BOTH halves. Guarding only the anchor lets a deleted row back in on the
	// second iteration, carrying its whole subtree with it — and the predicate
	// must be qualified to the CHILD, because both sides name the same table.
	if !live.Empty() {
		b.WriteString(" AND ")
		b.WriteString(string(LiveFor(recursiveChild, liveCol(live))))
	}
	b.WriteString(" AND NOT FIND_IN_SET(")
	b.WriteString(hexKey(c + "." + Ident(key)))
	b.WriteString(", ")
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
