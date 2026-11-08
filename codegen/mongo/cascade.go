// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package codegenmongo

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/compile/mongo"
	"github.com/gsoultan/storm/schema"
)

// cascadeNode is one collection of a delete's referential plan.
type cascadeNode struct {
	t     *schema.Table
	edges []string // rendered mgcascade.Edge literals
}

// cascadePlan is the referential actions a delete of root performs: every
// reference INTO root that says ON DELETE CASCADE, SET NULL, SET DEFAULT or
// RESTRICT, and, through each CASCADE, the references into its child. Nil
// when there is none, and then a delete is one command, as before.
func cascadePlan(s *schema.Schema, root *schema.Table) ([]cascadeNode, error) {
	if s == nil {
		return nil, nil
	}
	index := map[string]int{root.Name: 0}
	nodes := []cascadeNode{{t: root}}
	expanded := map[string]bool{}
	queue := []string{root.Name}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if expanded[name] {
			continue
		}
		expanded[name] = true
		parent := s.Table(name)
		var edges []string
		for _, child := range s.Tables {
			for _, fk := range child.ForeignKeys {
				act := actionOf(fk.OnDelete)
				if fk.RefTable != name || act == "" {
					continue
				}
				if r := mongo.EmbedOf(child); r != nil && len(fk.Columns) == 1 && fk.Columns[0] == r.Column {
					// Embedded: the rows are in the parent's document, and
					// go with it.
					continue
				}
				ci, ok := index[child.Name]
				if !ok {
					ci = len(nodes)
					index[child.Name] = ci
					nodes = append(nodes, cascadeNode{t: child})
				}
				if act == "Cascade" {
					queue = append(queue, child.Name)
				}
				set, err := setStage(child, fk)
				if err != nil {
					return nil, err
				}
				edges = append(edges, fmt.Sprintf("{Child: %d, Name: %q, From: %s, To: %s, Action: mgcascade.%s, Set: %q}",
					ci, refName(child.Name, fk.Columns),
					strList(mongo.Fields(child, fk.Columns)), strList(mongo.Fields(parent, fk.RefColumns)), act, set))
			}
		}
		nodes[index[name]].edges = edges
	}
	if len(nodes[0].edges) == 0 {
		return nil, nil
	}
	return nodes, nil
}

func actionOf(a schema.Action) string {
	switch a {
	case schema.Cascade:
		return "Cascade"
	case schema.SetNull:
		return "SetNull"
	case schema.SetDefault:
		return "SetDefault"
	case schema.Restrict:
		return "Restrict"
	}
	return ""
}

// setStage is the pipeline SET NULL or SET DEFAULT applies to a child.
func setStage(child *schema.Table, fk *schema.ForeignKey) (string, error) {
	switch fk.OnDelete {
	case schema.SetNull, schema.SetDefault:
	default:
		return "", nil
	}
	var parts []string
	for _, col := range fk.Columns {
		v := "null"
		if fk.OnDelete == schema.SetDefault {
			c := child.Column(col)
			switch k, lit := mongo.ClassifyDefault(c); k {
			case mongo.DefaultNone:
			case mongo.DefaultLiteral:
				v = `{"$literal":` + lit + `}`
			case mongo.DefaultServerTime:
				v = `"$$NOW"`
			default:
				return "", fmt.Errorf("codegenmongo: %s.%s: ON DELETE SET DEFAULT needs a default the server can write, not %s",
					child.Name, col, c.Default)
			}
		}
		parts = append(parts, mongo.Str(mongo.Field(child, col))+":"+v)
	}
	return `[{"$set":{` + strings.Join(parts, ",") + `}}]`, nil
}

func strList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = strconv.Quote(s)
	}
	return "[]string{" + strings.Join(q, ", ") + "}"
}

// cascadeVar emits the plan, or nothing.
func (g *gen) cascadeVar() {
	if g.plan == nil {
		return
	}
	g.p("// cascade is what deleting a document here does to the collections that")
	g.p("// reference it, as the model declared — MongoDB has no foreign keys, so")
	g.p("// storm performs the actions itself, in one transaction when it can open")
	g.p("// or join one. See runtime/mgcascade.")
	g.p("var cascade = &mgcascade.Plan{Nodes: []mgcascade.Node{")
	for _, n := range g.plan {
		if len(n.edges) == 0 {
			g.p("\t{Coll: %q},", n.t.Name)
			continue
		}
		g.p("\t{Coll: %q, Edges: []mgcascade.Edge{", n.t.Name)
		for _, e := range n.edges {
			g.p("\t\t%s,", e)
		}
		g.p("\t}},")
	}
	g.p("}}")
	g.p("")
	g.p("// keyFilter addresses one document by its key, for the plan's first read.")
	g.p("const keyFilter = %q", mongo.KeyFilter(g.t, 1))
	g.p("")
}

// refName is a reference as an error names it: "books.author_id", or
// "holds.(branch, barcode)" for a composite one.
func refName(table string, cols []string) string {
	if len(cols) == 1 {
		return table + "." + cols[0]
	}
	return table + ".(" + strings.Join(cols, ", ") + ")"
}
