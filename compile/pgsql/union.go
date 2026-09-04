package pgsql

import (
	"strings"

	"github.com/gsoultan/storm/schema"
)

// UnionSelect renders a declared union: every branch, merged, ordered as one.
//
// The whole statement is fixed at generation time — there is no call-site
// WHERE to splice. A union's rows come from different tables, so a predicate
// over it would have to say which branch it filtered, and the shape that
// question implies is not one storm can name (ADR-0008). Branch filters are
// declared; the only thing that varies per call is the paging, which the
// suffix carries.
//
// Each branch is parenthesised. PostgreSQL binds ORDER BY and LIMIT to the
// whole union rather than the last branch, but the reader of a generated
// statement should not have to know that to be sure.
func UnionSelect(u *schema.Union) string {
	var b strings.Builder

	sep := " UNION ALL "
	if u.Distinct {
		sep = " UNION "
	}

	for i := range u.Branches {
		if i > 0 {
			b.WriteString(sep)
		}
		writeUnionBranch(&b, u, &u.Branches[i])
	}
	return b.String()
}

func writeUnionBranch(b *strings.Builder, u *schema.Union, br *schema.UnionBranch) {
	b.WriteString("(SELECT ")
	for i, e := range br.Exprs {
		if i > 0 {
			b.WriteString(", ")
		}
		writeExpr(b, e)
		b.WriteString(" AS ")
		b.WriteString(Ident(ColumnCase(u.Cols[i].As)))
	}
	b.WriteString(" FROM ")
	b.WriteString(Ident(br.Table))
	if br.Where != nil {
		b.WriteString(" WHERE ")
		writeCond(b, *br.Where)
	}
	b.WriteByte(')')
}

// UnionSuffix is the ordering and the row cap, applied to the merged rows
// rather than to any one branch.
//
// The ORDER BY names OUTPUT columns, not table columns: after the merge the
// branches' own names are gone, and an output alias is the only thing in scope.
//
// The LIMIT is here rather than in codegen because `LIMIT` is not universal —
// SQL Server spells it `OFFSET … FETCH` and Oracle spelled it nothing at all
// until 12c. Every dialect word storm emits lives in its back end (R9), and
// `scripts/check/boundaries.sh` fails the build when one escapes.
//
// $1 without counting: a union carries no other bound value, because it has no
// call-site predicates to bind.
func UnionSuffix(u *schema.Union) string {
	var b strings.Builder
	if len(u.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, o := range u.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(Ident(ColumnCase(o.Col)))
			if o.Desc {
				b.WriteString(" DESC")
			}
			if o.NullsFirst != nil {
				if *o.NullsFirst {
					b.WriteString(" NULLS FIRST")
				} else {
					b.WriteString(" NULLS LAST")
				}
			}
		}
	}
	b.WriteString(" LIMIT " + Placeholder + "1")
	return b.String()
}
