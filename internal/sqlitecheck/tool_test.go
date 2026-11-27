// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlitecheck

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gsoultan/storm/v2"
	"github.com/gsoultan/storm/v2/runtime/sqlitedrv"
	"github.com/gsoultan/storm/v2/tool"
)

// The CLI, end to end on a database file: `storm diff` writes a migration,
// the migration is applied as any runner would, `storm verify` finds no
// drift, `storm verify -pending` replays the directory into memory and finds
// nothing pending, a changed model makes a SECOND migration that rebuilds a
// table, with a down beside it, and `storm import` reads it all back.
func TestTheCLIMigratesASQLiteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	out := filepath.Join(dir, "migrations")
	db, err := sql.Open(sqlitedrv.DriverName, sqlitedrv.DSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) error {
		t.Helper()
		return tool.Run(append(args, "-dialect", "sqlite", "-dsn", path, "-out", out))
	}
	applyNew := func(seen map[string]bool) {
		t.Helper()
		files, _ := filepath.Glob(filepath.Join(out, "*.up.sql"))
		sort.Strings(files)
		for _, f := range files {
			if seen[f] {
				continue
			}
			seen[f] = true
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(string(b)); err != nil {
				t.Fatalf("applying %s: %v\n%s", filepath.Base(f), err, b)
			}
		}
	}
	seen := map[string]bool{}

	tool.Models = []any{&rtOrg{}, &rtUser{}, &rtCounter{}}
	defer func() { tool.Models = nil }()

	if err := run("portable", "sqlite"); err != nil {
		t.Fatalf("portable: %v", err)
	}
	if err := run("ddl"); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	if err := run("diff", "init"); err != nil {
		t.Fatalf("diff init: %v", err)
	}
	applyNew(seen)
	if err := run("verify"); err != nil {
		t.Fatalf("verify after applying the first migration: %v", err)
	}
	if err := run("verify", "-pending"); err != nil {
		t.Fatalf("verify -pending: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO rt_orgs (id, name) VALUES (x'01', 'acme')`); err != nil {
		t.Fatal(err)
	}

	// A model change SQLite has no ALTER for: a counter gains a NOT NULL
	// column with an expression default, and loses none of its rows.
	tool.Models = []any{&rtOrg{}, &rtUser{}, &rtCounterV2{}}
	if _, err := db.Exec(`INSERT INTO rt_counters (label) VALUES ('kept')`); err != nil {
		t.Fatal(err)
	}
	if err := run("diff", "label_upper", "-down"); err != nil {
		t.Fatalf("diff after a model change: %v", err)
	}
	ups, _ := filepath.Glob(filepath.Join(out, "*label_upper.up.sql"))
	if len(ups) != 1 {
		t.Fatalf("expected one new up migration, got %v", ups)
	}
	b, _ := os.ReadFile(ups[0])
	if !strings.Contains(string(b), "_storm_rebuild_rt_counters") || !strings.Contains(string(b), "PRAGMA foreign_keys = OFF") {
		t.Fatalf("the migration is not a guarded rebuild:\n%s", b)
	}
	if downs, _ := filepath.Glob(filepath.Join(out, "*label_upper.down.sql")); len(downs) != 1 {
		t.Fatalf("no down migration beside it: %v", downs)
	}
	applyNew(seen)
	if err := run("verify"); err != nil {
		t.Fatalf("verify after the rebuild: %v", err)
	}
	var label, shout string
	if err := db.QueryRow(`SELECT label, shout FROM rt_counters`).Scan(&label, &shout); err != nil {
		t.Fatal(err)
	}
	if label != "kept" || shout != "x" {
		t.Errorf("the rebuild lost the row: label=%q shout=%q", label, shout)
	}
	if err := run("import"); err != nil {
		t.Fatalf("import: %v", err)
	}
}

type rtCounterV2 struct {
	ID    int64
	Label string
	Shout string
}

func (c *rtCounterV2) Schema(t *storm.Table) {
	t.Name("rt_counters")
	t.PrimaryKey(&c.ID)
	t.Col(&c.ID).Identity()
	t.Col(&c.Shout).Default("lower('X')")
}
