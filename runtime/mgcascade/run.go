// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgcascade

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/bsondec"
)

// ErrTooDeep is a cascade past MaxDepth levels — a reference cycle in the
// data.
var ErrTooDeep = errors.New("storm: a cascade went deeper than 64 levels; the references form a cycle")

// chunk bounds the values one child command matches on.
const chunk = 1000

// Delete deletes the documents of the plan's first collection that filter —
// a JSON filter document whose holes args bind — matches, performing every
// reference's action first; del is the delete (or the command that removes
// them), bound with args. It reports what del reports.
func Delete(ctx context.Context, ex runtime.Executor, p *Plan, filter string, args []any, del string) (int64, error) {
	var n int64
	run := func(ex runtime.Executor) error {
		var err error
		n, err = p.remove(ctx, ex, 0, filter, args, del, 0)
		return err
	}
	if _, ok := ex.(runtime.Tx); ok {
		return n, run(ex)
	}
	if db, ok := ex.(runtime.DB); ok {
		err := runtime.InTx(ctx, db, run)
		return n, err
	}
	return n, run(ex)
}

func (p *Plan) remove(ctx context.Context, ex runtime.Executor, node int, filter string, args []any, del string, depth int) (int64, error) {
	if depth > MaxDepth {
		return 0, ErrTooDeep
	}
	nd := &p.Nodes[node]
	if len(nd.Edges) > 0 {
		vals, err := referenced(ctx, ex, nd, filter, args)
		if err != nil {
			return 0, err
		}
		// Every RESTRICT first: a refusal must come before anything changed,
		// which matters most when there is no transaction to undo it.
		for i, e := range nd.Edges {
			if e.Action != Restrict {
				continue
			}
			for _, part := range split(vals[i]) {
				f, a := match(e.From, part)
				found, err := exists(ctx, ex, p.Nodes[e.Child].Coll, f, a)
				if err != nil {
					return 0, err
				}
				if found {
					return 0, errors.Join(runtime.ErrForeignKeyViolation,
						errors.New("storm: "+e.Name+" still references a document being deleted (ON DELETE RESTRICT)"))
				}
			}
		}
		for i, e := range nd.Edges {
			child := p.Nodes[e.Child].Coll
			for _, part := range split(vals[i]) {
				f, a := match(e.From, part)
				var err error
				switch e.Action {
				case Cascade:
					_, err = p.remove(ctx, ex, e.Child, f, a,
						`{"delete":`+quote(child)+`,"deletes":[{"limit":0,"q":`+f+`}]}`, depth+1)
				case SetNull, SetDefault:
					_, err = ex.Exec(ctx, `{"update":`+quote(child)+`,"updates":[{"multi":true,"q":`+f+`,"u":`+e.Set+`}]}`, a)
				}
				if err != nil {
					return 0, err
				}
			}
		}
	}
	return ex.Exec(ctx, del, args)
}

// referenced reads, for each edge, the distinct tuples of the parent's
// referenced fields across the documents filter matches. A tuple with a
// missing field references nothing, as a NULL key does in SQL.
func referenced(ctx context.Context, ex runtime.Executor, nd *Node, filter string, args []any) ([][][]bsondec.Raw, error) {
	var fields []string
	at := map[string]int{}
	for _, e := range nd.Edges {
		for _, f := range e.To {
			if _, ok := at[f]; !ok {
				at[f] = len(fields)
				fields = append(fields, f)
			}
		}
	}
	var proj strings.Builder
	proj.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			proj.WriteByte(',')
		}
		proj.WriteString(quote(f))
		proj.WriteString(":1")
	}
	proj.WriteByte('}')
	rows, err := ex.Query(ctx, `{"find":`+quote(nd.Coll)+`,"projection":`+proj.String()+`,"filter":`+filter+`}`, args)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([][][]bsondec.Raw, len(nd.Edges))
	seen := make([]map[string]bool, len(nd.Edges))
	for i := range seen {
		seen[i] = map[string]bool{}
	}
	for rows.Next() {
		rv := rows.RawValues()
	edges:
		for i, e := range nd.Edges {
			tuple := make([]bsondec.Raw, len(e.To))
			var key strings.Builder
			for j, f := range e.To {
				v := rv[at[f]]
				if v == nil {
					continue edges
				}
				tuple[j] = bsondec.Raw(append([]byte(nil), v...))
				key.WriteString(strconv.Itoa(len(v)))
				key.WriteByte(':')
				key.Write(v)
			}
			if k := key.String(); !seen[i][k] {
				seen[i][k] = true
				out[i] = append(out[i], tuple)
			}
		}
	}
	return out, rows.Err()
}

// split cuts tuples into chunks a command can carry.
func split(tuples [][]bsondec.Raw) [][][]bsondec.Raw {
	var out [][][]bsondec.Raw
	for len(tuples) > chunk {
		out = append(out, tuples[:chunk])
		tuples = tuples[chunk:]
	}
	if len(tuples) > 0 {
		out = append(out, tuples)
	}
	return out
}

// match is the filter that finds the children holding any of the tuples in
// from: one $in for a single field, an $or of equalities for several.
func match(from []string, tuples [][]bsondec.Raw) (string, []any) {
	if len(from) == 1 {
		vals := make([]bsondec.Raw, len(tuples))
		for i, t := range tuples {
			vals[i] = t[0]
		}
		return `{` + quote(from[0]) + `:{"$in":{"$arg":1}}}`, []any{vals}
	}
	var b strings.Builder
	args := make([]any, 0, len(tuples)*len(from))
	b.WriteString(`{"$or":[`)
	for i, t := range tuples {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('{')
		for j, f := range from {
			if j > 0 {
				b.WriteByte(',')
			}
			args = append(args, t[j])
			b.WriteString(quote(f))
			b.WriteString(`:{"$arg":`)
			b.WriteString(strconv.Itoa(len(args)))
			b.WriteByte('}')
		}
		b.WriteByte('}')
	}
	b.WriteString(`]}`)
	return b.String(), args
}

func exists(ctx context.Context, ex runtime.Executor, coll, filter string, args []any) (bool, error) {
	rows, err := ex.Query(ctx, `{"find":`+quote(coll)+`,"limit":1,"projection":{"_id":1},"filter":`+filter+`}`, args)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := rows.Next()
	return found, rows.Err()
}

// quote is a field or collection name as a JSON string. Names come from the
// model, never from a caller, but a quote or backslash still must not end the
// string early.
func quote(s string) string {
	if !strings.ContainsAny(s, "\"\\") {
		return `"` + s + `"`
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
