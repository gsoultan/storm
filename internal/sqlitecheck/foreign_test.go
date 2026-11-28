// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlitecheck

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/v2/migrate"
	"github.com/gsoultan/storm/v2/schema"
)

// A database storm did NOT make, compared with the model an adopter writes for
// it. An application that supports PostgreSQL and SQLite from one set of
// models declares each column the way PostgreSQL needs it — bigint, double,
// timestamp, a unique constraint under PostgreSQL's own name — and its SQLite
// DDL says what SQLite programmers write: INTEGER, REAL, TIMESTAMP, an inline
// UNIQUE, a TEXT PRIMARY KEY without NOT NULL.
//
// Every one of those differences is a label: SQLite stores and enforces both
// sides identically. Through v2.1.0 each was a change, and the plan rebuilt the
// table to change nothing. This is the shape gateon's schema has, which is how
// it was found.

const foreignDDL = `CREATE TABLE api_tokens (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	uses INTEGER NOT NULL DEFAULT 0,
	ratio REAL NOT NULL DEFAULT 0,
	created_at TIMESTAMP NOT NULL,
	seen_at DATETIME
)`

// foreignModel is what PostgreSQL would introspect for the same table.
func foreignModel() *schema.Schema {
	t := &schema.Table{
		Name: "api_tokens",
		Columns: []*schema.Column{
			col("id", schema.TypeText, true),
			col("name", schema.TypeText, true),
			col("token_hash", schema.TypeText, true),
			{Name: "uses", Type: schema.Type{Name: schema.TypeInt8}, NotNull: true, Default: "0"},
			{Name: "ratio", Type: schema.Type{Name: schema.TypeFloat8}, NotNull: true, Default: "0"},
			col("created_at", schema.TypeTimestamp, true),
			col("seen_at", schema.TypeTimestamp, false),
		},
		PrimaryKey: []string{"id"},
		Uniques:    []*schema.Unique{{Name: "api_tokens_token_hash_key", Columns: []string{"token_hash"}}},
	}
	return sch(t)
}

func foreignPlan(t *testing.T, model *schema.Schema) migrate.Plan {
	t.Helper()
	_, cn := conn(t)
	if _, err := cn.Exec(ctxBG(), foreignDDL, nil); err != nil {
		t.Fatal(err)
	}
	plan, err := migrate.ForSQLite(ctxBG(), cn, model, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestAForeignDatabaseThatDiffersOnlyInLabelsDiffsAsNothing(t *testing.T) {
	if plan := foreignPlan(t, foreignModel()); !plan.Empty() {
		t.Fatalf("labels SQLite stores identically produced a plan:\n%s", plan.SQL())
	}
}

// The fold must not swallow a difference that changes answers. Each case is
// one real change to the model; the plan must carry it.
func TestAForeignDatabaseStillDiffsWhatChangesAnswers(t *testing.T) {
	cases := map[string]struct {
		edit func(*schema.Table)
		want string
	}{
		"integer becomes text": {
			func(t *schema.Table) { t.Column("uses").Type = schema.Type{Name: schema.TypeText} },
			"uses",
		},
		"timestamp becomes a date": {
			func(t *schema.Table) { t.Column("created_at").Type = schema.Type{Name: schema.TypeDate} },
			"created_at",
		},
		"a column outside the key becomes NOT NULL": {
			func(t *schema.Table) { t.Column("seen_at").NotNull = true },
			"seen_at",
		},
		"the unique moves to another column": {
			func(t *schema.Table) { t.Uniques[0].Columns = []string{"name"} },
			"name",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			model := foreignModel()
			c.edit(model.Tables[0])
			plan := foreignPlan(t, model)
			if plan.Empty() {
				t.Fatal("a change that alters answers produced no plan")
			}
			if !strings.Contains(plan.SQL(), c.want) {
				t.Errorf("the plan does not mention %s:\n%s", c.want, plan.SQL())
			}
		})
	}
}
