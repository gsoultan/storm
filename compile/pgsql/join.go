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
func JoinSelect(table string, j *schema.Join, aggFor func(cte schema.CTE) (prefix, suffix string)) string {
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
func JoinDeclaredWhere(j *schema.Join) string {
	if j.Where == nil {
		return ""
	}
	var b strings.Builder
	writeCond(&b, *j.Where)
	return b.String()
}
