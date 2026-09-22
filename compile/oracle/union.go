package oracle

import (
	"strings"

	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
)

// columnCase is the output-name convention, shared rather than restated: an
// alias is part of the generated Go type's field name, and two spellings of it
// would be two different structs for one declaration.
func columnCase(s string) string { return pgsql.ColumnCase(s) }

// Declared UNION reads.
//
// A union has no driving table (ADR-0008), so it hangs off the schema and every
// branch names its own. The shape is standard SQL and carries over whole here —
// including the two things SQL Server had to change.
//
// A declared parameter may be used in more than one branch: `:1` is a NAME, so
// two branches mentioning it bind one value passed once, the property
// PostgreSQL's `$1` has and MySQL's `?` cannot (position is what binds there).
//
// And the ordering's NULL placement is spelled rather than refused — Oracle has
// NULLS FIRST/LAST wherever an ORDER BY appears, so UnionOrderRefused has
// nothing to report.

// UnionSelect renders the branches. live gives each branch's table its
// soft-delete predicate, per BRANCH rather than per union: the branches read
// different tables and only some of them may soft-delete, so a predicate
// hoisted to the whole union would name a column half of them do not have.
func UnionSelect(u *schema.Union, live func(table string) Live) (string, error) {
	var b strings.Builder
	sep := " UNION ALL "
	if u.Distinct {
		sep = " UNION "
	}
	var w exprWriter
	for i := range u.Branches {
		if i > 0 {
			b.WriteString(sep)
		}
		writeUnionBranch(&b, &w, u, &u.Branches[i], live)
	}
	return b.String(), w.err
}

func writeUnionBranch(b *strings.Builder, w *exprWriter, u *schema.Union,
	br *schema.UnionBranch, live func(table string) Live) {
	b.WriteString("(SELECT ")
	for i, e := range br.Exprs {
		if i > 0 {
			b.WriteString(", ")
		}
		w.b.Reset()
		w.writeExpr(e)
		b.WriteString(w.b.String())
		b.WriteString(" AS ")
		b.WriteString(Ident(columnCase(u.Cols[i].As)))
	}
	b.WriteString(" FROM ")
	b.WriteString(Ident(br.Table))
	hasWhere := br.Where != nil
	if hasWhere {
		b.WriteString(" WHERE ")
		w.b.Reset()
		w.writeCond(*br.Where)
		b.WriteString(w.b.String())
	}
	if live != nil {
		live(br.Table).AndInto(b, hasWhere)
	}
	b.WriteByte(')')
}

// UnionSuffix is the ordering and the row cap, applied to the merged rows.
//
// The ORDER BY names OUTPUT columns: after the merge the branches' own names
// are gone and an output alias is the only thing in scope.
//
// Unlike SQL Server, a union with no declared ordering gets NONE invented:
// FETCH FIRST is not a clause of ORDER BY here, so there is nothing to hang the
// cap on and nothing to make up. OrderFallback is empty for that reason.
func UnionSuffix(u *schema.Union) string {
	var b strings.Builder
	if len(u.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, o := range u.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(Ident(columnCase(o.Col)))
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
	// The cap is the last placeholder, after every declared parameter — the
	// order the generated function takes them.
	b.WriteString(" FETCH FIRST " + Param(len(u.Params)+1) + " ROWS ONLY")
	return b.String()
}

// UnionOrderRefused reports an ordering Oracle cannot express.
//
// Always nil, and that is the finding rather than an oversight: Oracle has
// NULLS FIRST / NULLS LAST wherever an ORDER BY appears, so unlike SQL Server
// there is no placement to refuse. The function stays because the seam has the
// shape, and a back end answering "nothing" is not the same as a back end the
// seam does not ask.
func UnionOrderRefused(*schema.Union) error { return nil }
