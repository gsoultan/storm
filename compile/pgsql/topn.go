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
func TopNWindow(table string, cols []string, key string, live Live) string {
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
	b.WriteString("1)")
	// Inside the subquery, beside the key filter: a marked row must not occupy
	// one of the N slots the window function hands out, or a parent with N
	// deleted children gets an empty page instead of its live ones.
	live.AndInto(&b, true)
	b.WriteString(") ")
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
	// Before the LIMIT, so a parent whose most recent N children are all
	// deleted still gets its live ones rather than an empty page.
	live.AndInto(&b, true)
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
	rowNumberAlias = "_storm_rn"
	subqueryAlias  = "_storm_t"
	lateralAlias   = "_storm_c"
	parentAlias    = "_storm_p"
	parentKeyAlias = "_storm_k"

	turnsOnward  = "_storm_onward"
	turnsWrapped = "_storm_wrapped"
	turnsKey     = "_storm_g"
	turnsDepth   = "_storm_n"
	turnsGroups  = "_storm_groups"
	turnsFound   = "_storm_found"
	turnsQueued  = "_storm_queued"
	turnsTurn    = "_storm_turn"
)

func writeIdents(b *strings.Builder, cols []string) {
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(Ident(c))
	}
}

// Turns lowers a read that deals a table's rows out by group, a turn at a
// time: each group's first rows numbered in the read's order, and every
// group's first turn ahead of anyone's second. A queue claim that must not
// let one tenant's backlog starve the rest is this read.
//
// The groups are found by skipping along group a seek at a time, from the
// first at or after $1 and wrapping round to the start, at most $2 of them,
// so the read costs what the groups it looks at cost and not what the table
// holds; an index on the groups' predicates' columns and then group is what
// makes each seek one. Each group's rows are one range: its first $3, in the
// order, numbered. When group is nullable the rows with none are one more
// group. At most $3 rows come back in all.
//
// The text is returned around the splices the caller makes. head opens the
// statement. groups are the four reads of the group column, each a declared
// predicate and the text after the splice: the caller splices the groups'
// own predicates into each. rows are the reads of the rows, likewise, with
// the rows' predicates and order spliced in, and their last close ends the
// statement. Every close that numbers the rows carries orderMarker twice in
// all: the window's ordering, then the final one's, after the turn.
func Turns(table, group string, cols []string, nullable bool, live Live) (head string, groups [4][2]string, rows [][2]string) {
	t, g := Ident(table), Ident(group)
	on, wr := Ident(turnsOnward), Ident(turnsWrapped)
	k, n := Ident(turnsKey), Ident(turnsDepth)
	var b strings.Builder
	writeIdents(&b, cols)
	sel := b.String()
	and := func(cond string) string {
		if live == "" {
			return cond
		}
		return cond + " AND " + string(live)
	}
	seek := " ORDER BY " + g + " LIMIT 1)"
	step := func(cte string) string {
		return ", " + cte + "." + n + " + 1 FROM " + cte + " WHERE " + cte + "." + k + " IS NOT NULL AND " +
			cte + "." + n + " < " + Placeholder + "2)"
	}
	first := "(SELECT " + g + ", 1 FROM " + t
	next := " UNION ALL SELECT (SELECT " + g + " FROM " + t
	head = "WITH RECURSIVE " + on + "(" + k + ", " + n + ") AS (" + first
	groups[0] = [2]string{and(g + " >= " + Placeholder + "1"), seek + next}
	groups[1] = [2]string{and(g + " > " + on + "." + k), seek + step(on) + ", " + wr + "(" + k + ", " + n + ") AS (" + first}
	groups[2] = [2]string{and(g + " < " + Placeholder + "1"), seek + next}
	found := Ident(turnsGroups)
	numbered := "SELECT " + sel + ", row_number() OVER (" + orderMarker + ") AS " + Ident(turnsTurn) + " FROM " + t
	groups[3] = [2]string{
		and(g + " > " + wr + "." + k + " AND " + g + " < " + Placeholder + "1"),
		seek + step(wr) + ", " + found + " AS (SELECT " + k + " FROM (SELECT " + k + " FROM " + on +
			" WHERE " + k + " IS NOT NULL UNION ALL SELECT " + k + " FROM " + wr + " WHERE " + k +
			" IS NOT NULL) AS " + Ident(turnsFound) + " LIMIT " + Placeholder + "2), " + Ident(turnsQueued) +
			" AS (SELECT " + Ident(lateralAlias) + ".* FROM " + found + " CROSS JOIN LATERAL (" + numbered,
	}
	end := " SELECT " + sel + ", " + Ident(turnsTurn) + " FROM " + Ident(turnsQueued) + " ORDER BY " +
		Ident(turnsTurn) + orderMarker + " LIMIT " + Placeholder + "3"
	limit := " LIMIT " + Placeholder + "3"
	if !nullable {
		rows = [][2]string{{and(g + " = " + found + "." + k), limit + ") AS " + Ident(lateralAlias) + ")" + end}}
		return head, groups, rows
	}
	rows = [][2]string{
		{and(g + " = " + found + "." + k), limit + ") AS " + Ident(lateralAlias) + " UNION ALL (" + numbered},
		{and(g + " IS NULL"), limit + "))" + end},
	}
	return head, groups, rows
}
