package mysql

import (
	"strings"

	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
)

// columnCase is pgsql.ColumnCase, called rather than copied.
//
// It is a storm naming convention and not a PostgreSQL fact — it turns an
// exported Go identifier into the snake_case the schema uses — and its own doc
// says why this is not reimplemented here: "two implementations of one rule is
// how they drift apart". It lives in compile/pgsql only because that is where
// the first back end needed it.
func columnCase(s string) string { return pgsql.ColumnCase(s) }

// Declared UNION reads.
//
// A union has no driving table (ADR-0008), so it hangs off the schema and every
// branch names its own. The shape is standard SQL and carries over; what does
// not is the ordering's null placement, and the placeholders — which are bare
// here, so a parameter used in two branches is two bindings rather than one.
// compile/mysql/expr.go refuses that rather than silently changing the arity.

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
// No NULLS placement — MySQL has none, and a union's ordering is the caller's
// paging order, so dropping an explicit one would page differently rather than
// spell differently. UnionOrderRefused reports that before it is emitted.
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
		}
	}
	// The cap is the last placeholder, after every declared parameter — the
	// order the generated function takes them.
	b.WriteString(" LIMIT " + Placeholder)
	return b.String()
}

// UnionOrderRefused reports an ordering MySQL cannot express, or nil.
func UnionOrderRefused(u *schema.Union) error {
	for _, o := range u.OrderBy {
		if o.NullsFirst != nil {
			return ErrNoNullsPlacement
		}
	}
	return nil
}
