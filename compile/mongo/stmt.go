// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mongo

import (
	"strings"

	"github.com/gsoultan/storm/schema"
)

// The command templates. Each read is split around the two places the splicer
// writes — the filter and the sort — and numbers its own holes first, so the
// predicates' holes start at a fixed ordinal whatever the shape.

// FindHead opens a find. Holes 1 and 2 are the row cap and the skip; the
// predicates are numbered from FindFirstArg. The filter follows directly.
func FindHead(coll string, fields []string) string {
	return `{"find":` + Str(coll) + `,"limit":` + Arg(1) + `,"skip":` + Arg(2) +
		`,"projection":` + Projection(fields) + `,"filter":`
}

// FindFirstArg is the first predicate hole of a find.
const FindFirstArg = 3

// FindSort sits between the filter and the sort document.
const FindSort = `,"sort":`

// FindTail closes a find.
const FindTail = `}`

// ExistsHead is a find for one key and nothing else, with no sort: asking
// whether a row exists must not pay for an ordering nobody reads.
func ExistsHead(coll string) string {
	return `{"find":` + Str(coll) + `,"limit":1,"projection":{"_id":1},"filter":`
}

// ExistsTail closes ExistsHead. Predicates are numbered from 1.
const ExistsTail = `}`

// CountHead is an aggregation that counts what the filter matches. The result
// has one field, n, and no row at all when nothing matched — which the
// generated Count reads as zero.
func CountHead(coll string) string {
	return `{"aggregate":` + Str(coll) + `,"cursor":{},"pipeline":[{"$match":`
}

// CountTail closes CountHead.
const CountTail = `},{"$count":"n"}]}`

// DeleteHead deletes every document the filter matches. Predicates from 1.
func DeleteHead(coll string) string {
	return `{"delete":` + Str(coll) + `,"deletes":[{"limit":0,"q":`
}

// DeleteTail closes DeleteHead.
const DeleteTail = `}]}`

// UpdateHead updates every document the filter matches with u, a rendered
// update document whose holes the caller numbered from 1. Predicates follow.
func UpdateHead(coll, u string) string {
	return `{"update":` + Str(coll) + `,"updates":[{"multi":true,"u":` + u + `,"q":`
}

// UpdateTail closes UpdateHead.
const UpdateTail = `}]}`

// Assign is one field of an update document, by operator.
type Assign struct {
	// Op is "$set", "$setOnInsert", "$inc" or "$currentDate".
	Op    string
	Field string
	// Arg is the hole; 0 for $currentDate, whose value is fixed, and for a
	// literal.
	Arg int
	// Lit is a literal JSON value written in place of a hole — a column
	// default the model declared as a constant.
	Lit string
}

// UpdateDoc renders assignments grouped by operator, in first-appearance
// order, so a statement's text is a function of its mask alone.
func UpdateDoc(as []Assign) string {
	var ops []string
	by := map[string][]Assign{}
	for _, a := range as {
		if _, ok := by[a.Op]; !ok {
			ops = append(ops, a.Op)
		}
		by[a.Op] = append(by[a.Op], a)
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, op := range ops {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(Str(op))
		b.WriteString(":{")
		for j, a := range by[op] {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(Str(a.Field))
			b.WriteByte(':')
			switch {
			case a.Op == "$currentDate":
				b.WriteString(`{"$type":"date"}`)
			case a.Lit != "":
				b.WriteString(a.Lit)
			default:
				b.WriteString(Arg(a.Arg))
			}
		}
		b.WriteByte('}')
	}
	if len(ops) == 0 {
		// An update document must name at least one operator; this one
		// changes nothing and is only reached by an Update that assigned
		// nothing, which the generated code never sends.
		b.WriteString(`"$set":{}`)
	}
	b.WriteByte('}')
	return b.String()
}

// InsertOne inserts one document and reads it back in the same command.
//
// findAndModify with upsert, against a filter that matches nothing: the upsert
// path takes its _id from the filter's equality and fails with a duplicate
// key if the key exists, so this is an INSERT — never an overwrite — that can
// also apply $currentDate. That is what lets now() stay the SERVER's clock, as
// it is on every SQL target, and hand back the stored document the way
// RETURNING does. `$nor: [{}]` is the predicate that matches nothing: an empty
// filter matches everything, and its negation nothing.
//
// The key's holes come first, in key order; u's holes follow.
func InsertOne(t *schema.Table, u string, fields []string) string {
	return `{"findAndModify":` + Str(t.Name) + `,"query":{"_id":` + KeyValue(t, 1) +
		`,"$nor":[{}]},"update":` + u + `,"upsert":true,"new":true,"fields":` +
		Projection(fields) + `}`
}

// UpdateOne applies u to the document with key hole 1 and returns it as it now
// stands — the read-back the SQL targets get from RETURNING, in one command.
// No document matched means no row, which is how a missing key is reported.
func UpdateOne(t *schema.Table, u string, fields []string) string {
	return `{"findAndModify":` + Str(t.Name) + `,"query":` + KeyFilter(t, 1) +
		`,"update":` + u + `,"new":true,"fields":` + Projection(fields) + `}`
}

// DeleteOne deletes the document with this key, whose values are the first
// holes.
func DeleteOne(t *schema.Table) string {
	return `{"delete":` + Str(t.Name) + `,"deletes":[{"limit":1,"q":` + KeyFilter(t, 1) + `}]}`
}

// GetOne reads the document with this key.
func GetOne(t *schema.Table, fields []string) string {
	return `{"find":` + Str(t.Name) + `,"limit":1,"projection":` + Projection(fields) +
		`,"filter":` + KeyFilter(t, 1) + `}`
}
