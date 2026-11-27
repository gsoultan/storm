// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/caduceus/wire"
	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/schema"
	"github.com/gsoultan/storm/schema/pg"
)

// explain plans every statement storm will issue and flags sequential scans
// the planner expects to be large.
//
// Two jobs, honestly separated:
//
// As a VALIDITY gate it needs no data: every generated statement must plan,
// which catches a shape whose SQL PostgreSQL rejects — for all of them, on
// every CI run, via GENERIC_PLAN (PostgreSQL 16+), which plans a parameterised
// statement without inventing bind values.
//
// As a PERFORMANCE gate it is only as good as the statistics it runs against.
// On a CI database with ten rows the planner prefers a sequential scan for
// everything and is RIGHT to, so seq scans are flagged only when the planner's
// own row estimate crosses -max-seq-rows — which on an empty database is
// never. Run it against a stats-bearing replica for the real signal; the
// threshold exists so that run fails loudly.
func explain(dsn, ns string, model *schema.Schema, maxSeqRows int) error {
	var names []string
	for _, t := range model.Tables {
		names = append(names, t.Name)
	}
	queries, err := codegen.ExplainQueries(model, names)
	if err != nil {
		return err
	}

	// SET takes no parameter, so the namespace reaches SQL text here. It is a
	// flag the operator typed, not request data, but it is held to the rule
	// Introspect holds it to rather than trusted.
	if err := schemapg.ValidNamespace(ns); err != nil {
		return err
	}
	c, ctx, done, err := connect(dsn)
	if err != nil {
		return err
	}
	defer done()
	if _, err := c.Exec(ctx, "SET search_path TO "+ns); err != nil {
		return err
	}

	flagged := 0
	for _, q := range queries {
		scans, err := planOne(ctx, c, q.SQL, maxSeqRows)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %-28s does not plan: %v\n      %s\n", q.Label, err, q.SQL)
			flagged++
			continue
		}
		if len(scans) == 0 {
			fmt.Printf("  ✓ %-28s planned\n", q.Label)
			continue
		}
		flagged++
		for _, s := range scans {
			fmt.Fprintf(os.Stderr, "  ✗ %-28s seq scan on %s, ~%.0f rows\n", q.Label, s.table, s.rows)
		}
	}
	if flagged > 0 {
		return fmt.Errorf("%d statement(s) flagged — add the index the WHERE clause wants, "+
			"or raise -max-seq-rows if the scan is intended", flagged)
	}
	fmt.Printf("✓ %d statement(s) planned, none seq-scans past %d rows\n", len(queries), maxSeqRows)
	return nil
}

type seqScan struct {
	table string
	rows  float64
}

func planOne(ctx context.Context, c *caduceus.Conn, sql string, maxSeqRows int) ([]seqScan, error) {
	// The raw wire, deliberately. GENERIC_PLAN exists to plan a statement with
	// placeholders and NO values, and the extended protocol binds a value to
	// every $n it parses, so a simple Query message is the only way to send it:
	// the text goes untouched and the server does what GENERIC_PLAN is for.
	// Safe here because nothing in the statement is a runtime value — that is
	// the property being audited.
	raw, err := simpleFirstValue(ctx, c, "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "+sql)
	if err != nil {
		return nil, err
	}
	var doc []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) == 0 {
		return nil, fmt.Errorf("unexpected EXPLAIN output: %w", err)
	}
	return walkPlan(doc[0].Plan, maxSeqRows), nil
}

// simpleFirstValue runs sql over the simple query protocol and returns the
// first column of its first row.
func simpleFirstValue(ctx context.Context, c *caduceus.Conn, sql string) ([]byte, error) {
	if c.Busy() {
		return nil, caduceus.ErrConnBusy
	}
	w := c.Wire()
	guard := w.Watch(ctx)
	first, err := readSimple(w, sql)
	return first, guard.Done(err)
}

// readSimple sends one simple Query and reads its replies to ReadyForQuery.
func readSimple(w *wire.Conn, sql string) ([]byte, error) {
	if err := w.Send(wire.AppendQuery(w.Buffer(), sql)); err != nil {
		return nil, err
	}
	var first []byte
	var failure error
	for {
		kind, body, err := w.Receive()
		if err != nil {
			return nil, err
		}
		switch kind {
		case wire.MsgDataRow:
			if first == nil {
				values, err := wire.SplitDataRow(nil, body)
				if err != nil {
					return nil, err
				}
				if len(values) > 0 {
					first = append([]byte{}, values[0]...)
				}
			}
		case wire.MsgErrorResponse:
			if failure == nil {
				failure = wire.ParseError(body)
			}
		case wire.MsgReadyForQuery:
			if failure != nil {
				return nil, failure
			}
			if first == nil {
				return nil, fmt.Errorf("EXPLAIN returned no rows")
			}
			return first, nil
		}
	}
}

// walkPlan collects sequential scans whose estimated rows cross the threshold.
func walkPlan(raw json.RawMessage, maxSeqRows int) []seqScan {
	var node struct {
		NodeType string            `json:"Node Type"`
		Relation string            `json:"Relation Name"`
		PlanRows float64           `json:"Plan Rows"`
		Plans    []json.RawMessage `json:"Plans"`
	}
	if json.Unmarshal(raw, &node) != nil {
		return nil
	}
	var out []seqScan
	if node.NodeType == "Seq Scan" && node.PlanRows >= float64(maxSeqRows) {
		out = append(out, seqScan{table: node.Relation, rows: node.PlanRows})
	}
	for _, child := range node.Plans {
		out = append(out, walkPlan(child, maxSeqRows)...)
	}
	return out
}
