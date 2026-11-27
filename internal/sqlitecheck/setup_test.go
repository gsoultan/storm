// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package sqlitecheck runs storm's SQLite back end against a real SQLite.
//
// SQLite needs no server, so unlike the other live gates nothing here skips:
// every test opens a fresh database in a temporary directory, and CI runs the
// whole module on every push. The driver is modernc.org/sqlite, a pure-Go
// translation of SQLite, so the run needs no C toolchain either.
package sqlitecheck

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/storm/v2/runtime/sqlitedrv"

	_ "modernc.org/sqlite"
)

// open is a fresh file database with storm's connection settings.
func open(t testing.TB) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "storm.db")
	db, err := sql.Open(sqlitedrv.DriverName, sqlitedrv.DSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// mustExec fails the test, naming the statement.
func mustExec(t testing.TB, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s\n  %v", q, err)
	}
}

// apply runs a list of statements.
func apply(t testing.TB, db *sql.DB, stmts []string) {
	t.Helper()
	for _, st := range stmts {
		if strings.TrimSpace(st) == "" {
			continue
		}
		mustExec(t, db, st)
	}
}
