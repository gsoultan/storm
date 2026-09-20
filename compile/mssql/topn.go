package mssql

import "strings"

// Greatest-n-per-group: "each parent with its first N children".
//
// The naive answer is one query per parent, which is the N+1 this library
// exists to make unrepresentable. Both forms below stay at ONE query.
//
// This is where the dialects genuinely part company. PostgreSQL binds a whole
// parent-key list as ONE array parameter and unnests it; MySQL has no array
// type and uses JSON_TABLE over a bound document (ADR-0010). SQL Server has no
// array type either, and OPENJSON is its JSON_TABLE — one bound document,
// statement text independent of how many keys the caller passed.
//
// The one thing OPENJSON does NOT need is MySQL's hex round trip. A uuid key is
// UNIQUEIDENTIFIER here, not BINARY(16), so it travels through JSON as its own
// canonical text and comes back as itself — no HEX on the way out and no UNHEX
// on the way in, and therefore no expression wrapped around the indexed column.

// orderMarker is where the ordering goes. It is runtime.OrderMarker, spelled
// here so compile/ does not import runtime.
const orderMarker = "\x00order\x00"

const (
	rowNumberAlias = "_storm_rn"
	subqueryAlias  = "_storm_t"
	lateralAlias   = "_storm_c"
	parentAlias    = "_storm_p"
	parentKeyAlias = "_storm_k"
)

// keyRows is the parent-key list as a relation: one bound JSON document,
// unpacked by OPENJSON into a column of the key's own type.
//
// The WITH declaration must be TYPED to the key it will be joined against.
// Left to OPENJSON's default schema every value comes back NVARCHAR(4000), and
// comparing that to a UNIQUEIDENTIFIER or a BIGINT puts an implicit conversion
// on the COLUMN side of the predicate — which SQL Server will happily do, and
// which makes the index unusable. Losing the index is the whole reason this
// form was chosen over reading every child of every parent.
func keyRows(keyType string) string {
	return "OPENJSON(" + Param(1) + ") WITH (" +
		Ident(parentKeyAlias) + " " + keyType + " '$') AS " + Ident(parentAlias)
}

// keyValue is the unpacked key, ready to be compared against the indexed
// column. qualify names the derived table where the reference needs it; empty
// is an unqualified reference, which is what a subquery in an IN takes.
func keyValue(keyType, qualify string) string {
	if qualify == "" {
		return Ident(parentKeyAlias)
	}
	return Ident(qualify) + "." + Ident(parentKeyAlias)
}

// TopNWindow lowers greatest-n-per-group with row_number().
//
// It reads every matching child, numbers them within each parent and discards
// the ones past N. NOT the default, for the reason PostgreSQL's is not: its
// cost tracks the total child count rather than the rows returned. Kept because
// a caller whose data defeats the APPLY plan needs a way out that does not
// involve writing SQL.
func TopNWindow(table string, cols []string, key, keyType string, live Live) string {
	var b strings.Builder
	b.WriteString("SELECT ")
	writeIdents(&b, cols)
	b.WriteString(" FROM (SELECT ")
	writeIdents(&b, cols)
	b.WriteString(", row_number() OVER (PARTITION BY ")
	b.WriteString(Ident(key))
	b.WriteString(orderMarker)
	b.WriteString(") AS ")
	b.WriteString(Ident(rowNumberAlias))
	b.WriteString(" FROM ")
	b.WriteString(Ident(table))
	b.WriteString(" WHERE ")
	b.WriteString(Ident(key))
	b.WriteString(" IN (SELECT ")
	b.WriteString(keyValue(keyType, ""))
	b.WriteString(" FROM ")
	b.WriteString(keyRows(keyType))
	b.WriteString(")")
	// A marked row must not occupy one of the N slots the window hands out, or
	// a parent whose recent children are all deleted gets an empty page.
	live.AndInto(&b, true)
	b.WriteString(") AS ")
	b.WriteString(Ident(subqueryAlias))
	b.WriteString(" WHERE ")
	b.WriteString(Ident(rowNumberAlias))
	b.WriteString(" <= ")
	b.WriteString(Param(2))
	return b.String()
}

// TopNLateral lowers greatest-n-per-group with CROSS APPLY, which is SQL
// Server's LATERAL. THE DEFAULT, for the reason PostgreSQL's is: a limited,
// ordered scan per parent key, which the planner stops at N rows.
//
// The row cap is TOP, and TOP comes before the column list rather than after
// the ordering — so the cap's parameter is the second in the text, after the
// key document's, which is the same order the LIMIT forms produce. The
// generated caller binds keys then cap either way.
//
// TOP takes a parenthesised expression: `TOP (@p2)`. Without the parentheses a
// parameter is a syntax error, and only a literal is accepted.
func TopNLateral(table string, cols []string, key, keyType string, live Live) string {
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(Ident(lateralAlias))
		b.WriteString(".")
		b.WriteString(Ident(c))
	}
	b.WriteString(" FROM ")
	b.WriteString(keyRows(keyType))
	b.WriteString(" CROSS APPLY (SELECT TOP (")
	b.WriteString(Param(2))
	b.WriteString(") ")
	writeIdents(&b, cols)
	b.WriteString(" FROM ")
	b.WriteString(Ident(table))
	b.WriteString(" WHERE ")
	b.WriteString(Ident(key))
	b.WriteString(" = ")
	b.WriteString(keyValue(keyType, parentAlias))
	// Before the ordering, so a parent whose most recent N children are deleted
	// still gets its live ones rather than an empty page.
	live.AndInto(&b, true)
	b.WriteString(orderMarker)
	b.WriteString(") AS ")
	b.WriteString(Ident(lateralAlias))
	return b.String()
}

func writeIdents(b *strings.Builder, cols []string) {
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(Ident(c))
	}
}
