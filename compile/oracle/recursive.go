package oracle

import "strings"

// Recursive traversal of a self-reference: a whole subtree or ancestor chain in
// one query rather than one query per level.
//
// Oracle has recursive CTEs, spelled WITHOUT the RECURSIVE keyword — a CTE
// that names itself simply is one — and it has something none of the other
// three targets do: a CYCLE clause that detects a repeated key server-side.
//
// That removes the machinery every other back end needs. PostgreSQL carries the
// visited keys in an array, MySQL in a CHAR(4000) that silently truncates, and
// SQL Server in an NVARCHAR(MAX) whose type the anchor has to CAST so the two
// members agree. Here the server tracks it: no path column, no hex encoding to
// keep a separator out of a key, no delimiter search per iteration, and nothing
// to overflow. MaxRecursionDepth is 0 for that reason and not the usual one.
//
// The column list is declared on the CTE name rather than aliased inside it,
// because CYCLE names a column and needs it in scope.

// Traversal directions, numbered as the generated Query numbers them.
const (
	Descend = iota // children of the given roots
	Ascend         // ancestors of the given rows
)

const (
	recursiveAlias = "_storm_r"
	recursiveChild = "_storm_rc"
	depthAlias     = "_storm_d"
	cycleAlias     = "_storm_cycle"
)

// Recursive lowers a self-referential traversal.
//
// THE CYCLE GUARD IS THE SERVER'S, which is why this is the shortest of the
// four. PostgreSQL accumulates visited keys in an array, MySQL in a CHAR(4000)
// that can silently truncate, and SQL Server in an NVARCHAR(MAX) it has to
// CAST in the anchor so the two members' types agree. Oracle has `CYCLE key SET
// flag TO 'Y' DEFAULT 'N'` built into the recursive WITH: it detects a repeated
// key itself and marks the row rather than looping.
//
// So there is no path column, no hex encoding to keep a separator out of a key,
// no delimiter search per iteration, and nothing to overflow. The traversal
// stops at a cycle and the marked row is filtered out.
//
// keyType is the SQL type of the key, needed because the roots arrive as a
// bound JSON document that JSON_TABLE unpacks — Oracle has no array parameter
// to compare against directly (ADR-0010).
func Recursive(table string, cols []string, key, parent, keyType string, dir int, live Live) string {
	var b strings.Builder
	t := Ident(recursiveAlias)
	c := Ident(recursiveChild)

	b.WriteString("WITH ")
	b.WriteString(t)
	b.WriteString("(")
	writeIdents(&b, cols)
	b.WriteString(", ")
	b.WriteString(Ident(depthAlias))
	b.WriteString(") AS (SELECT ")
	writeIdents(&b, cols)
	b.WriteString(", 1 FROM ")
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
	b.WriteString(" + 1 FROM ")
	// Juxtaposition, not AS: `FROM t AS x` is ORA-00933.
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
	b.WriteString(Param(2))
	// BOTH halves. Guarding only the anchor lets a deleted row back in on the
	// second iteration, carrying its whole subtree with it — and the predicate
	// must be qualified to the CHILD, because both sides name the same table.
	if !live.Empty() {
		b.WriteString(" AND ")
		b.WriteString(LiveFor(recursiveChild, liveCol(live)))
	}
	b.WriteString(")")

	// The guard itself, and the only reason this back end needs no path.
	// CYCLE comes after the CTE body and before the outer SELECT.
	b.WriteString(" CYCLE ")
	b.WriteString(Ident(key))
	b.WriteString(" SET ")
	b.WriteString(Ident(cycleAlias))
	b.WriteString(" TO 'Y' DEFAULT 'N' SELECT ")
	writeIdents(&b, cols)
	b.WriteString(" FROM ")
	b.WriteString(t)
	// The marked row is the one that WOULD have looped. It is excluded rather
	// than returned, which is what every other back end's path check does by
	// never emitting it in the first place.
	b.WriteString(" WHERE ")
	b.WriteString(Ident(cycleAlias))
	b.WriteString(" = 'N'")
	return b.String()
}
