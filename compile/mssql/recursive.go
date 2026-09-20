package mssql

import "strings"

// Recursive traversal of a self-reference: a whole subtree or ancestor chain in
// one query rather than one query per level.
//
// SQL Server has recursive CTEs, spelled WITHOUT the RECURSIVE keyword — a CTE
// that references itself simply is one. What it does not have is PostgreSQL's
// array, so the CYCLE GUARD is a string path, as it is on MySQL.
//
// Two differences from MySQL's version, and both make this one safer:
//
//   - The path is NVARCHAR(MAX). MySQL infers a recursive CTE column's type
//     from the ANCHOR, so its path has to be CAST to a fixed CHAR(4000) and a
//     traversal deeper than the width silently truncates — a guard that stops
//     guarding. MAX has no width to overflow, so MaxRecursionDepth is 0 here:
//     there is no depth this guard stops holding at.
//   - The depth limit is the server's, and it REFUSES rather than truncates.
//     A recursive CTE stops at 100 levels unless the statement says otherwise,
//     and hitting the limit is an error, not a short answer. OPTION
//     (MAXRECURSION 0) lifts it, because the real bound is the caller's
//     maxDepth predicate below — bound as a parameter, which a literal in the
//     OPTION clause could never match.
//
// The path elements are hex, for the reason MySQL's are: a text key can contain
// the separator, and then the guard reads one visited key as two and stops
// guarding. CONVERT(…, VARBINARY(MAX), …) then style 2 is [0-9A-F] for every
// key type SQL Server has, so the separator cannot occur inside an element.

// Traversal directions, numbered as the generated Query numbers them.
const (
	Descend = iota // children of the given roots
	Ascend         // ancestors of the given rows
)

const (
	recursiveAlias = "_storm_r"
	recursiveChild = "_storm_rc"
	depthAlias     = "_storm_d"
	pathAlias      = "_storm_path"
)

// hexKey renders a key as hex text, so a path element can never contain the
// separator. Style 2 is hex with no leading 0x.
func hexKey(qualified string) string {
	return "CONVERT(NVARCHAR(MAX), CONVERT(VARBINARY(MAX), " + qualified + "), 2)"
}

// Recursive lowers a self-referential traversal.
//
// keyType is the SQL type of the key, needed because the roots arrive as a
// bound JSON document that OPENJSON unpacks — SQL Server has no array parameter
// to compare against directly.
func Recursive(table string, cols []string, key, parent, keyType string, dir int, live Live) string {
	var b strings.Builder
	t := Ident(recursiveAlias)
	c := Ident(recursiveChild)

	b.WriteString("WITH ")
	b.WriteString(t)
	b.WriteString(" AS (SELECT ")
	writeIdents(&b, cols)
	b.WriteString(", 1 AS ")
	b.WriteString(Ident(depthAlias))
	b.WriteString(", ")
	// CAST to MAX in the ANCHOR, because SQL Server requires the anchor and the
	// recursive member to agree on every column's type exactly — and the
	// recursive member concatenates, which only stays MAX if the anchor already
	// is. Without it the server refuses the CTE outright rather than
	// truncating, which is the better failure but still a failure.
	b.WriteString("CAST(" + hexKey(Ident(key)) + " AS NVARCHAR(MAX))")
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
	b.WriteString(" + 1, ")
	b.WriteString(t)
	b.WriteString(".")
	b.WriteString(Ident(pathAlias))
	b.WriteString(" + ',' + ")
	b.WriteString(hexKey(c + "." + Ident(key)))
	b.WriteString(" FROM ")
	b.WriteString(Ident(table))
	b.WriteString(" AS ")
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
	// CHARINDEX over a delimited path is FIND_IN_SET: the separators on both
	// sides of the needle are what stop one key matching the tail of another.
	b.WriteString(" AND CHARINDEX(',' + ")
	b.WriteString(hexKey(c + "." + Ident(key)))
	b.WriteString(" + ',', ',' + ")
	b.WriteString(t)
	b.WriteString(".")
	b.WriteString(Ident(pathAlias))
	b.WriteString(" + ',') = 0) SELECT ")
	writeIdents(&b, cols)
	b.WriteString(" FROM ")
	b.WriteString(t)
	// Last clause of the statement, which is where the grammar requires it.
	b.WriteString(" OPTION (MAXRECURSION 0)")
	return b.String()
}
