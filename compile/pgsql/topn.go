package pgsql

import "strings"

// Greatest-n-per-group: "each parent with its first N children".
//
// The naive answer is one query per parent, which is the N+1 this whole library
// exists to make unrepresentable. The two answers that stay at ONE query are
// below. Both are generated; which one is the default is decided by
// measurement, not by argument — see bench/RESULTS.md.
//
// A third form, DISTINCT ON, is deliberately absent. It only expresses N=1, so
// having it would mean a strategy that silently stops applying when a caller
// changes Top(1) to Top(2), and a per-parent limit that quietly stops being
// per-parent is worse than one that was never offered.

// TopNWindow lowers greatest-n-per-group with row_number().
//
// It reads every matching child, numbers them within each parent and discards
// the ones past N. NOT the default: measured, its cost tracks the total child
// count rather than the rows returned, and it loses to TopNLateral at every
// parent count and every N — by 33x at a hundred parents. Kept because a
// caller whose data defeats the lateral plan needs a way out that does not
// involve writing SQL, and because a default chosen by measurement needs
// something to have been measured against.
//
// cols are the child's projected columns and key is the partition column. The
// ordering is left as a marker for runtime.SpliceOrder, because it varies per
// call and sits inside the window clause rather than at the end. Two
// placeholders: the parent key array, then N.
func TopNWindow(table string, cols []string, key string) string {
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
	b.WriteString(" = ANY(")
	b.WriteString(Placeholder)
	b.WriteString("1)) ")
	b.WriteString(Ident(subqueryAlias))
	b.WriteString(" WHERE ")
	b.WriteString(Ident(rowNumberAlias))
	b.WriteString(" <= ")
	b.WriteString(Placeholder)
	b.WriteString("2")
	return b.String()
}

// TopNLateral lowers greatest-n-per-group with a lateral join. THE DEFAULT,
// by measurement.
//
// It unnests the parent keys into a relation and runs a limited, ordered scan
// per key. The planner stops each one at N rows, so with a matching index it
// touches only the rows it returns. It pays a nested-loop iteration per parent,
// which the window form does not — and that cost is dwarfed by not reading
// every child of every parent. See bench/RESULTS.md.
//
// keyType is the SQL type of the parent key, needed because an unnested array
// parameter has no type of its own.
func TopNLateral(table string, cols []string, key, keyType string) string {
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
	b.WriteString(" FROM unnest(")
	b.WriteString(Placeholder)
	b.WriteString("1::")
	b.WriteString(keyType)
	b.WriteString("[]) AS ")
	b.WriteString(Ident(parentAlias))
	b.WriteString("(")
	b.WriteString(Ident(parentKeyAlias))
	b.WriteString(") CROSS JOIN LATERAL (SELECT ")
	writeIdents(&b, cols)
	b.WriteString(" FROM ")
	b.WriteString(Ident(table))
	b.WriteString(" WHERE ")
	b.WriteString(Ident(key))
	b.WriteString(" = ")
	b.WriteString(Ident(parentAlias))
	b.WriteString(".")
	b.WriteString(Ident(parentKeyAlias))
	b.WriteString(orderMarker)
	b.WriteString(" LIMIT ")
	b.WriteString(Placeholder)
	b.WriteString("2) ")
	b.WriteString(Ident(lateralAlias))
	return b.String()
}

// Aliases the lowered forms introduce. They are prefixed so a column called
// "rn" or "p" in a real table cannot collide with one.
// orderMarker is where the ordering goes. It is runtime.OrderMarker, spelled
// here so compile/ does not import runtime.
const orderMarker = "\x00order\x00"

const (
	rowNumberAlias = "_raorm_rn"
	subqueryAlias  = "_raorm_t"
	lateralAlias   = "_raorm_c"
	parentAlias    = "_raorm_p"
	parentKeyAlias = "_raorm_k"
)

func writeIdents(b *strings.Builder, cols []string) {
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(Ident(c))
	}
}
