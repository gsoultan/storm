// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlite

import (
	"strings"

	"github.com/gsoultan/storm/v2/schema"
)

// AggregateSelect is everything before the WHERE clause of a grouped read.
//
// Split at WHERE for the same reason every other read is: the call-site
// predicates are dynamic and spliced in by the runtime, while this prefix is
// fixed at generation time and never rebuilt.
func AggregateSelect(table string, agg *schema.Aggregate) (string, error) {
	if agg.Sets != nil {
		return "", ErrNoGroupingSets
	}
	var w exprWriter
	w.b.WriteString("SELECT ")
	for i, g := range agg.By {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.writeExpr(g.Expr)
		w.b.WriteString(" AS ")
		w.b.WriteString(Ident(ColumnCase(g.As)))
	}
	for i, t := range agg.Terms {
		if i > 0 || len(agg.By) > 0 {
			w.b.WriteString(", ")
		}
		w.writeExpr(t.Expr)
		w.b.WriteString(" AS ")
		w.b.WriteString(Ident(ColumnCase(t.As)))
	}
	w.b.WriteString(" FROM ")
	w.b.WriteString(Ident(table))
	for _, j := range agg.Joins {
		w.writeAggJoin(j)
	}
	return w.b.String(), w.err
}

// writeAggJoin renders one of an aggregation's sources: a joined table, or a
// range over a JSON array's elements.
//
// The elements are read through json_each, a table-valued function SQLite
// joins as a correlated source without LATERAL. It is given the document only
// when the document IS an array: json_each over an object walks its members,
// over a scalar yields the scalar, and over malformed text raises — and one
// such row would fail the whole read. Such a row contributes no elements, as
// PostgreSQL's CASE makes it.
func (w *exprWriter) writeAggJoin(j schema.JoinTable) {
	if j.Kind == schema.JoinElements {
		if j.Of == nil {
			return
		}
		var of exprWriter
		of.writeExpr(*j.Of)
		if of.err != nil {
			w.fail(of.err)
			return
		}
		w.b.WriteString(" CROSS JOIN ")
		w.b.WriteString(JSONElements(of.b.String()))
		w.b.WriteString(" AS ")
		w.b.WriteString(Ident(j.Alias))
		return
	}
	if j.Kind == schema.JoinLeft {
		w.b.WriteString(" LEFT JOIN ")
	} else {
		w.b.WriteString(" JOIN ")
	}
	w.b.WriteString(Ident(j.Table))
	if j.Alias != "" && j.Alias != j.Table {
		w.b.WriteString(" AS ")
		w.b.WriteString(Ident(j.Alias))
	}
	w.b.WriteString(" ON ")
	w.writeCond(j.On)
}

// JSONElements is the element range over a document: json_each over it when
// it is a well-formed array, and over an empty one otherwise.
func JSONElements(doc string) string {
	return "json_each(CASE WHEN json_valid(" + doc + ") AND json_type(" + doc +
		") = 'array' THEN " + doc + " ELSE '[]' END)"
}

// AggregateWhere is the aggregation's declared predicate, or empty.
func AggregateWhere(agg *schema.Aggregate) (string, error) {
	if agg.Where == nil {
		return "", nil
	}
	return Cond(*agg.Where)
}

// AggregateSuffix is the GROUP BY, HAVING and ORDER BY that follow the
// predicates, in the order SQL wants them. See compile/pgsql for why a grouped
// read is always ordered, and why the grouping columns are appended as a
// tiebreak to an ordering by a measure.
func AggregateSuffix(agg *schema.Aggregate) (string, error) {
	group, err := GroupBy(agg)
	if err != nil {
		return "", err
	}
	var w exprWriter
	w.b.WriteString(group)

	if agg.Having != nil {
		w.b.WriteString(" HAVING ")
		w.writeCond(*agg.Having)
	}

	if len(agg.OrderBy) > 0 {
		w.b.WriteString(" ORDER BY ")
		for i, o := range agg.OrderBy {
			if i > 0 {
				w.b.WriteString(", ")
			}
			w.b.WriteString(Ident(ColumnCase(o.As)))
			if o.Desc {
				w.b.WriteString(" DESC")
			}
		}
		for _, g := range agg.By {
			if named(agg.OrderBy, g.As) {
				continue
			}
			w.b.WriteString(", ")
			w.b.WriteString(Ident(ColumnCase(g.As)))
		}
		return w.b.String(), w.err
	}

	if len(agg.By) > 0 {
		w.b.WriteString(" ORDER BY ")
		for i, g := range agg.By {
			if i > 0 {
				w.b.WriteString(", ")
			}
			w.b.WriteString(Ident(ColumnCase(g.As)))
		}
	}
	return w.b.String(), w.err
}

// named reports whether an ordering already mentions an output.
func named(order []schema.AggOrder, as string) bool {
	for _, o := range order {
		if o.As == as {
			return true
		}
	}
	return false
}

// ColumnCase turns an exported Go identifier into snake_case, matching the
// convention the rest of the schema uses for column names. Exported because
// codegen derives the same alias for its synthetic columns, and two
// implementations of one rule is how they drift apart.
func ColumnCase(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			// A boundary is a lower→upper transition, or an upper followed by
			// a lower — so "OrderID" is order_id and not order_i_d.
			if i > 0 && (isLower(s[i-1]) || (i+1 < len(s) && isLower(s[i+1]))) {
				b.WriteByte('_')
			}
			b.WriteByte(c - 'A' + 'a')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
