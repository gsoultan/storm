package mysql

import (
	"errors"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Declared cross-table reads.
//
// The shape is standard SQL and carries over: MySQL 8 has JOIN, LEFT JOIN,
// table aliases, and WITH. What does not carry over is what the CTEs CONTAIN —
// each one runs a declared aggregation, and aggregates have no MySQL lowering
// yet, so a join that uses one is refused rather than half-emitted.
//
// The placement that matters is the same as PostgreSQL's, and for the same
// reason: a joined table's soft-delete predicate goes in its ON clause, never
// the WHERE. In the WHERE, a parent whose only child is deleted is DROPPED and
// the outer join silently becomes an inner one.

// ErrCTEUnavailable is why a join with a materialised aggregation is refused.
var ErrCTEUnavailable = errors.New(
	"compile/mysql: this join materialises a declared aggregation as a CTE, and aggregates " +
		"have no MySQL lowering yet — MySQL has WITH, but not the FILTER and GROUPING SETS " +
		"forms an aggregation may carry. See docs/PLAN.md M9")

// JoinSelect renders the join: CTEs, projection, FROM and the attached tables.
//
// live gives a table its soft-delete predicate under the alias it is joined as.
// The alias matters: in `orders JOIN users AS u`, a bare `deleted_at IS NULL`
// is ambiguous when both tables have the column and wrong when only one does.
func JoinSelect(table string, j *schema.Join,
	aggFor func(cte schema.CTE) (prefix, suffix string),
	live func(table, alias string) Live) (string, error) {

	if len(j.CTEs) > 0 {
		return "", ErrCTEUnavailable
	}

	var b strings.Builder
	var w exprWriter

	b.WriteString("SELECT ")
	for i, c := range j.Select {
		if i > 0 {
			b.WriteString(", ")
		}
		w.b.Reset()
		w.writeExpr(c.Expr)
		b.WriteString(w.b.String())
		b.WriteString(" AS ")
		b.WriteString(Ident(columnCase(c.As)))
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
		if t.Table != "" && t.Table != t.Alias {
			b.WriteString(" AS ")
			b.WriteString(Ident(t.Alias))
		}
		b.WriteString(" ON ")
		w.b.Reset()
		w.writeCond(t.On)
		b.WriteString(w.b.String())

		// ON, not WHERE. For an inner join the two are equivalent; for a LEFT
		// JOIN they are not, and the difference is the whole behaviour of the
		// join.
		if live != nil && t.Table != "" {
			if p := live(t.Table, t.Alias); !p.Empty() {
				b.WriteString(" AND ")
				b.WriteString(string(p))
			}
		}
	}
	return b.String(), w.err
}

// JoinSuffix is the declared ORDER BY that follows the call-site predicates.
//
// No NULLS placement: MySQL has none, and a join's declared ordering is what
// the caller pages by, so dropping an explicit one would page differently
// rather than spell differently. JoinOrderRefused reports that first.
func JoinSuffix(j *schema.Join) (string, error) {
	var b strings.Builder
	var w exprWriter
	if len(j.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, o := range j.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			w.b.Reset()
			w.writeExpr(o.Expr)
			b.WriteString(w.b.String())
			if o.Desc {
				b.WriteString(" DESC")
			}
		}
	}
	return b.String(), w.err
}

// JoinDeclaredWhere renders the declared predicate the generator folds into the
// statement's fixed WHERE.
//
// driving is the predicate for the table the join reads FROM. That one belongs
// in the WHERE rather than an ON clause: it has no ON of its own, and excluding
// its marked rows is exactly what filtering the driving table should do.
func JoinDeclaredWhere(j *schema.Join, driving Live) (string, error) {
	var w exprWriter
	if j.Where != nil {
		w.writeCond(*j.Where)
	}
	return driving.And(w.b.String()), w.err
}

// And returns `where AND live`, or whichever of the two exists.
func (l Live) And(where string) string {
	switch {
	case l == "":
		return where
	case where == "":
		return string(l)
	default:
		return "(" + where + ") AND " + string(l)
	}
}
