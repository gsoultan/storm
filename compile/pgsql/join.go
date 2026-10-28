package pgsql

import (
	"strings"

	"github.com/gsoultan/storm/schema"
)

// JoinSelect is everything before the call-site WHERE of a cross-table read:
// the WITH clauses, the projection, the FROM and every JOIN.
//
// Split at WHERE like every other read, so the call-site predicates stay
// dynamic and this prefix is fixed at generation time.
func JoinSelect(table string, j *schema.Join, aggFor func(cte schema.CTE) (prefix, suffix string), live func(table, alias string) Live) string {
	var b strings.Builder
	for i, c := range j.CTEs {
		prefix, suffix := aggFor(c)
		head, tail := ctePiece(i, c.Alias, prefix, suffix)
		b.WriteString(head)
		b.WriteString(tail)
	}
	if len(j.CTEs) > 0 {
		b.WriteByte(' ')
	}
	b.WriteString(joinBody(table, j, live, false))
	return b.String()
}

// JoinPieces is JoinSelect cut where a narrowed CTE's own predicates go:
// each CTE's head — WITH or a comma, its name, its aggregation up to the WHERE
// — and tail — the aggregation's GROUP BY and the closing parenthesis — and
// then the main select, and the count of the same rows, each with a leading
// space.
func JoinPieces(table string, j *schema.Join, aggFor func(cte schema.CTE) (prefix, suffix string), live func(table, alias string) Live) (ctes [][2]string, sel, count string) {
	for i, c := range j.CTEs {
		prefix, suffix := aggFor(c)
		head, tail := ctePiece(i, c.Alias, prefix, suffix)
		ctes = append(ctes, [2]string{head, tail})
	}
	return ctes, " " + joinBody(table, j, live, false), " " + joinBody(table, j, live, true)
}

// ctePiece is one CTE around an aggregation's prefix and suffix.
func ctePiece(i int, alias, prefix, suffix string) (head, tail string) {
	lead := ", "
	if i == 0 {
		lead = "WITH "
	}
	return lead + Ident(alias) + " AS (" + prefix, suffix + ")"
}

// joinBody is the SELECT, the FROM and every JOIN; count selects count(*)
// instead of the declared outputs.
func joinBody(table string, j *schema.Join, live func(table, alias string) Live, count bool) string {
	var b strings.Builder
	b.WriteString("SELECT ")
	if count {
		b.WriteString("count(*)")
	}
	for i, c := range j.Select {
		if count {
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		writeExpr(&b, c.Expr)
		b.WriteString(" AS ")
		b.WriteString(Ident(ColumnCase(c.As)))
	}
	b.WriteString(" FROM ")
	b.WriteString(Ident(table))

	for _, t := range j.Tables {
		switch t.Kind {
		case schema.JoinLeft:
			b.WriteString(" LEFT JOIN ")
		default:
			b.WriteString(" JOIN ")
		}
		if t.Table != "" {
			b.WriteString(Ident(t.Table))
		} else {
			b.WriteString(Ident(t.Alias))
		}
		// A CTE is referred to by its alias, which IS its name; a table joined
		// under a different alias needs the AS.
		if t.Table != "" && t.Table != t.Alias {
			b.WriteString(" AS ")
			b.WriteString(Ident(t.Alias))
		}
		b.WriteString(" ON ")
		writeCond(&b, t.On)
		// ON, not WHERE. For an inner join the two are equivalent; for a LEFT
		// JOIN they are not, and the difference is the whole behaviour of the
		// join. In WHERE, a parent whose only child is deleted is DROPPED —
		// the outer join silently becomes an inner one. In ON, the parent
		// survives with a NULL-extended child, which is what "left join" was
		// asked for and what the same parent gets when it has no child at all.
		if live != nil && t.Table != "" {
			if p := live(t.Table, t.Alias); !p.Empty() {
				b.WriteString(" AND ")
				b.WriteString(string(p))
			}
		}
	}
	return b.String()
}

// JoinSuffix is the declared WHERE and ORDER BY that follow the call-site
// predicates.
//
// The declared WHERE is ANDed after them by the splice, so a declaration that
// says "only fulfilled orders" cannot be widened by a caller — which is the
// point of declaring it rather than leaving it to every call site.
func JoinSuffix(j *schema.Join) string {
	var b strings.Builder
	if len(j.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, o := range j.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(&b, o.Expr)
			if o.Desc {
				b.WriteString(" DESC")
			}
		}
	}
	return b.String()
}

// JoinDeclaredWhere renders the declared predicate, which the generator folds
// into the statement's fixed WHERE.
// driving is the predicate for the table the join reads FROM. That one belongs
// in the WHERE rather than an ON clause: it has no ON of its own, and excluding
// its marked rows is exactly what a filter on the driving table should do.
func JoinDeclaredWhere(j *schema.Join, driving Live) string {
	var b strings.Builder
	if j.Where != nil {
		writeCond(&b, *j.Where)
	}
	return driving.And(b.String())
}
