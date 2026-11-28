// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlite

import "strings"

// Greatest-n-per-group: "each parent with its first N children", in ONE query.
//
// PostgreSQL has two forms and picks the lateral one by measurement. SQLite
// has no LATERAL, so it has one: the window form, which numbers each parent's
// children with row_number() and keeps the first N. With an index on the
// parent key the read is one range scan per parent either way, and SQLite's
// planner walks the IN list's values against that index.

// TopNWindow lowers greatest-n-per-group with row_number().
//
// cols are the child's projected columns and key is the partition column. The
// ordering is left as a marker for runtime.SpliceOrder, because it varies per
// call and sits inside the window clause. Two parameters: the parent keys as a
// JSON array, then N.
func TopNWindow(table string, cols []string, key, keyType string, live Live) string {
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
	b.WriteString(" IN (SELECT ")
	b.WriteString(keyValue(keyType))
	b.WriteString(" FROM json_each(")
	b.WriteString(Placeholder)
	b.WriteString("1))")
	// Inside the subquery, beside the key filter: a marked row must not occupy
	// one of the N slots the window function hands out.
	live.AndInto(&b, true)
	b.WriteString(") AS ")
	b.WriteString(Ident(subqueryAlias))
	b.WriteString(" WHERE ")
	b.WriteString(Ident(rowNumberAlias))
	b.WriteString(" <= ")
	b.WriteString(Placeholder)
	b.WriteString("2")
	return b.String()
}

// orderMarker is where the ordering goes. It is runtime.OrderMarker, spelled
// here so compile/ does not import runtime.
const orderMarker = "\x00order\x00"

// Aliases the lowered forms introduce, prefixed so a real column called "rn"
// cannot collide with one.
const (
	rowNumberAlias = "_storm_rn"
	subqueryAlias  = "_storm_t"
)

func writeIdents(b *strings.Builder, cols []string) {
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(Ident(c))
	}
}
