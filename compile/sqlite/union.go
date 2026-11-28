// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlite

import (
	"strconv"
	"strings"

	"github.com/gsoultan/storm/v2/schema"
)

// UnionSelect renders a declared union: every branch, merged, ordered as one.
//
// See compile/pgsql for why the whole statement is fixed at generation time.
// The difference here is the parentheses: PostgreSQL's branches are wrapped,
// and a parenthesised SELECT in a compound is a syntax error in SQLite, whose
// ORDER BY and LIMIT always bind to the whole compound anyway.
func UnionSelect(u *schema.Union, live func(table string) Live) (string, error) {
	var w exprWriter
	sep := " UNION ALL "
	if u.Distinct {
		sep = " UNION "
	}
	for i := range u.Branches {
		if i > 0 {
			w.b.WriteString(sep)
		}
		w.writeUnionBranch(u, &u.Branches[i], live)
	}
	return w.b.String(), w.err
}

func (w *exprWriter) writeUnionBranch(u *schema.Union, br *schema.UnionBranch, live func(table string) Live) {
	w.b.WriteString("SELECT ")
	for i, e := range br.Exprs {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.writeExpr(e)
		w.b.WriteString(" AS ")
		w.b.WriteString(Ident(ColumnCase(u.Cols[i].As)))
	}
	w.b.WriteString(" FROM ")
	w.b.WriteString(Ident(br.Table))
	hasWhere := br.Where != nil
	if hasWhere {
		w.b.WriteString(" WHERE ")
		w.writeCond(*br.Where)
	}
	if live != nil {
		live(br.Table).AndInto(&w.b, hasWhere)
	}
}

// UnionSuffix is the ordering and the row cap, applied to the merged rows
// rather than to any one branch.
//
// The ORDER BY names OUTPUT columns, not table columns: after the merge the
// branches' own names are gone, and an output alias is the only thing in scope.
//
// The cap is numbered without counting: a union carries no other bound value
// than its declared parameters, because it has no call-site predicates.
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
	// The cap is the last placeholder, after every declared parameter — which
	// is also the order the generated function takes them.
	b.WriteString(" LIMIT " + Placeholder + strconv.Itoa(len(u.Params)+1))
	return b.String()
}
