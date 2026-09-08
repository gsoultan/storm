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

	if len(j.CTEs) > 0 {
		b.WriteString("WITH ")
		for i, c := range j.CTEs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(Ident(c.Alias))
			b.WriteString(" AS (")
			prefix, suffix := aggFor(c)
			b.WriteString(prefix)
			b.WriteString(suffix)
			b.WriteByte(')')
		}
		b.WriteByte(' ')
	}

	b.WriteString("SELECT ")
	for i, c := range j.Select {
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
		b.WriteString(Ident(t.Alias))
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
