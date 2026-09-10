package mysql

import "strings"

// Greatest-n-per-group: "each parent with its first N children".
//
// The naive answer is one query per parent, which is the N+1 this library
// exists to make unrepresentable. Both forms below stay at ONE query; which is
// the default is decided by measurement, not argument.
//
// This is the construct M9's exit gate names — "the JSON_TABLE batch loader" —
// and it is where the dialects genuinely part company. PostgreSQL binds a whole
// parent-key list as ONE array parameter and unnests it. MySQL has no array
// type and no unnest, so the key list arrives as a bound JSON document that
// JSON_TABLE turns back into rows (ADR-0010). The alternative was `IN (?,?,?)`,
// whose arity depends on the caller's data — a statement shape keyed by request
// data rather than by the program, which is storm being an interpreter with
// extra steps.

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
// unpacked by JSON_TABLE into a column of the key's own type.
//
// The COLUMNS declaration must be TYPED to the key it will be joined against.
// Declared JSON it compares a JSON scalar to a native value — wrong, and
// unindexable — and losing the index is the whole reason this form was chosen
// over reading every child of every parent.
func keyRows(keyType string) string {
	return "JSON_TABLE(" + Placeholder + ", '$[*]' COLUMNS (" +
		Ident(parentKeyAlias) + " " + keyType + " PATH '$')) AS " + Ident(parentAlias)
}

// TopNWindow lowers greatest-n-per-group with row_number().
//
// It reads every matching child, numbers them within each parent and discards
// the ones past N. NOT the default, for the reason PostgreSQL's is not: its
// cost tracks the total child count rather than the rows returned. Kept because
// a caller whose data defeats the lateral plan needs a way out that does not
// involve writing SQL.
//
// MySQL 8 has window functions, so the shape carries over unchanged apart from
// how the parent keys arrive.
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
	b.WriteString(Ident(parentKeyAlias))
	b.WriteString(" FROM ")
	b.WriteString(keyRows(keyType))
	b.WriteString(")")
	// A marked row must not occupy one of the N slots the window hands out, or
	// a parent whose recent children are all deleted gets an empty page.
	live.AndInto(&b, true)
	b.WriteString(") ")
	b.WriteString(Ident(subqueryAlias))
	b.WriteString(" WHERE ")
	b.WriteString(Ident(rowNumberAlias))
	b.WriteString(" <= ")
	b.WriteString(Placeholder)
	return b.String()
}

// TopNLateral lowers greatest-n-per-group with a lateral join. THE DEFAULT on
// PostgreSQL by measurement; kept as the default here because the shape is the
// same one — a limited, ordered scan per parent key, which the planner stops at
// N rows — and because it is the form ADR-0010 measured on MySQL 8.4.11 and saw
// reach the index.
//
// MySQL 8.0.14+ has LATERAL. keyType is the SQL type of the parent key, needed
// because a JSON-unpacked value has no type of its own.
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
	b.WriteString(" CROSS JOIN LATERAL (SELECT ")
	writeIdents(&b, cols)
	b.WriteString(" FROM ")
	b.WriteString(Ident(table))
	b.WriteString(" WHERE ")
	b.WriteString(Ident(key))
	b.WriteString(" = ")
	b.WriteString(Ident(parentAlias))
	b.WriteString(".")
	b.WriteString(Ident(parentKeyAlias))
	// Before the LIMIT, so a parent whose most recent N children are deleted
	// still gets its live ones rather than an empty page.
	live.AndInto(&b, true)
	b.WriteString(orderMarker)
	b.WriteString(" LIMIT ")
	b.WriteString(Placeholder)
	b.WriteString(") ")
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

// AndInto appends the soft-delete predicate to a WHERE clause being written.
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
