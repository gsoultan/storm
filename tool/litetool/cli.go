// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package litetool is the SQLite half of the storm CLI: import, diff, verify
// and the pending check, each against a database FILE.
//
// A package of its own for the reason tool/oratool is one. It is the
// smallest of them, because SQLite needs no server: the -dsn is a path, the
// pending check replays the migrations into an in-memory database, and the
// tests run everywhere.
package litetool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gsoultan/storm/v2/codegen"
	"github.com/gsoultan/storm/v2/migrate"
	"github.com/gsoultan/storm/v2/runtime/sqlitedrv"
	"github.com/gsoultan/storm/v2/schema"
	"github.com/gsoultan/storm/v2/schema/sqlite"
)

// DriverPath is the database/sql driver storm's SQLite support is written and
// tested against: pure Go, so a build needs no C toolchain.
const DriverPath = "modernc.org/sqlite"

// missingDriver turns database/sql's "unknown driver" into an instruction.
func missingDriver(err error) error {
	if !strings.Contains(err.Error(), "unknown driver") {
		return err
	}
	return fmt.Errorf("%w\n"+
		"       storm reaches SQLite through database/sql, and the DRIVER is yours to\n"+
		"       choose. Add it to the file that calls tool.Main:\n\n"+
		"           import _ %q\n\n"+
		"       then: go get %s\n"+
		"       (`storm` run without a hand-written bootstrap adds this itself)",
		err, DriverPath, DriverPath)
}

// DSN is the connection string for -dsn: a bare path gets storm's connection
// settings (foreign keys on, case-sensitive LIKE, a busy timeout — see
// sqlitedrv.DSN), and a string that already carries options is used as it is.
func DSN(dsn string) string {
	if strings.Contains(dsn, "?") {
		return dsn
	}
	return sqlitedrv.DSN(strings.TrimPrefix(dsn, "file:"))
}

// Open connects and returns ONE connection, which is what migrate's SQLite
// entry points need: an attached database and a PRAGMA belong to the
// connection that ran them.
func Open(ctx context.Context, dsn string) (migrate.Conn, func(), error) {
	if dsn == "" {
		return nil, nil, errors.New("this reads a live database: pass -dsn path/to/app.db (or set $STORM_DSN)")
	}
	if !strings.Contains(dsn, ":memory:") && !strings.Contains(dsn, "mode=memory") {
		path := strings.TrimPrefix(dsn, "file:")
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		if _, err := os.Stat(path); err != nil {
			// SQLite would CREATE the file, and an import or a diff against an
			// empty database is a wrong answer rather than an error.
			return nil, nil, fmt.Errorf("no database at %s: %w", path, err)
		}
	}
	return open(ctx, DSN(dsn))
}

func open(ctx context.Context, dsn string) (migrate.Conn, func(), error) {
	db, err := sql.Open(sqlitedrv.DriverName, dsn)
	if err != nil {
		return nil, nil, missingDriver(err)
	}
	c, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, nil, missingDriver(err)
	}
	return sqlitedrv.New(c), func() { c.Close(); db.Close() }, nil
}

// ImportModel is `storm import` against SQLite: the Go model the database
// implies.
func ImportModel(ctx context.Context, dsn, modulePath string) ([]byte, error) {
	c, done, err := Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer done()
	s, err := schemasqlite.Introspect(ctx, c, "")
	if err != nil {
		return nil, err
	}
	return codegen.Model(s, codegen.ModelOptions{Package: "model", Import: modulePath})
}

// Plan is the migration from the database at dsn to the model.
func Plan(ctx context.Context, dsn string, model *schema.Schema, o migrate.Options) (migrate.Plan, error) {
	c, done, err := Open(ctx, dsn)
	if err != nil {
		return migrate.Plan{}, err
	}
	defer done()
	return migrate.ForSQLite(ctx, c, model, o)
}

// ReplayAndDiff applies the migration files, in order, to an in-memory
// database and reports what the model still wants. No -dsn is needed: the
// scratch database is the process's own.
func ReplayAndDiff(ctx context.Context, files []string, model *schema.Schema, o migrate.Options) (migrate.Plan, error) {
	c, done, err := open(ctx, sqlitedrv.DSN(":memory:"))
	if err != nil {
		return migrate.Plan{}, err
	}
	defer done()
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return migrate.Plan{}, err
		}
		if _, err := c.Exec(ctx, string(b), nil); err != nil {
			return migrate.Plan{}, fmt.Errorf("replaying %s: %w", filepath.Base(f), err)
		}
	}
	return migrate.ForSQLite(ctx, c, model, o)
}
