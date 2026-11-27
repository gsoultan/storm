// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/storm/v2/compile/liteddl"
	"github.com/gsoultan/storm/v2/runtime"
	"github.com/gsoultan/storm/v2/runtime/sqlitedrv"
	"github.com/gsoultan/storm/v2/runtime/valdec"
	"github.com/gsoultan/storm/v2/schema"
	"github.com/gsoultan/storm/v2/schema/sqlite"
)

// SQLite: the seam, the table rebuild, normalisation and automigrate.
//
// SQLITE HAS FIVE ALTERS. Rename a table, rename a column, add a column (with
// restrictions), drop a column (with more), and the index statements. Every
// other change the differ can ask for — a column's type, nullability or
// default; a table's key, uniques, checks or foreign keys; an identity; a
// generated expression — is made by building the table again: create it as
// it should be under another name, copy the rows, drop the old one, rename.
// compile/liteddl.Rebuild renders that.
//
// So the seam renders in two passes. The renderers below never write a
// rebuild themselves: each one that has no ALTER returns a MARKER naming the
// table, and diffFor's last step, rebuildTables, replaces every table's markers
// with ONE rebuild that makes all of its changes at once, at the place in the
// plan of the last of them. A table with a dropped column, a new check and a
// retyped column is rebuilt once, not three times — and the steps that do have
// an ALTER on that table, an index or an added column, go with it, since the
// rebuild creates the table and its indexes whole. Renames and backfills stay
// where they were, ahead of it: the rebuild copies by the names the renames
// leave, and the backfill is what lets a tightened column's rows survive the
// copy.
//
// THE DESTRUCTIVE GUARD survives the merge. Each step the differ marked
// destructive stays in the plan as a comment, with its Why and its Object, in
// front of the rebuild that carries it out — so AllowDestructive and Accept see
// exactly the steps they would on any other server, one by one, and a plan
// that drops a column is refused for that and not for "rebuilds a table".

// SQLite is the dialect constant.
const SQLite Dialect = "sqlite"

// A marker is a renderer's answer when it has no single statement: the kind,
// the table, and a description of the change, NUL-separated so no SQL can
// collide with one. inPlace markers also carry the statement to run when the
// table is not rebuilt.
const (
	markRebuild = "\x00rebuild\x00"
	markInPlace = "\x00inplace\x00"
)

func rebuildMark(t *schema.Table, what string) string {
	return markRebuild + t.Name + "\x00" + what
}

func inPlaceMark(t *schema.Table, sql string) string {
	return markInPlace + t.Name + "\x00" + sql
}

// parseMark splits a marker, or reports that s is ordinary SQL.
func parseMark(s string) (kind, table, body string, ok bool) {
	switch {
	case strings.HasPrefix(s, markRebuild):
		kind, s = "rebuild", s[len(markRebuild):]
	case strings.HasPrefix(s, markInPlace):
		kind, s = "inplace", s[len(markInPlace):]
	default:
		return "", "", "", false
	}
	i := strings.IndexByte(s, 0)
	if i < 0 {
		return "", "", "", false
	}
	return kind, s[:i], s[i+1:], true
}

func sqliteDDL(enums map[string]*schema.Enum) ddl {
	q := liteddl.Ident
	alter := func(t *schema.Table, rest string) string { return "ALTER TABLE " + q(t.Name) + " " + rest }

	d := ddl{
		Ident: q,

		CreateTable: func(t *schema.Table) (string, error) {
			def, err := liteddl.CreateTable(t, enums)
			if err != nil {
				return "", err
			}
			return strings.Join(append([]string{def}, liteddl.Indexes(t)...), ";\n"), nil
		},
		DropTable: func(t *schema.Table) string { return "DROP TABLE " + q(t.Name) },
		AddForeignKey: func(t *schema.Table, fk *schema.ForeignKey) string {
			return rebuildMark(t, alter(t, "ADD "+liteddl.ForeignKeyDef(fk)))
		},

		AddColumn: func(t *schema.Table, c *schema.Column) (string, error) {
			def, err := liteddl.ColumnDef(t, c, enums)
			if err != nil {
				return "", err
			}
			if liteddl.AddsInPlace(t, c) {
				return inPlaceMark(t, alter(t, "ADD COLUMN "+def)), nil
			}
			return rebuildMark(t, alter(t, "ADD COLUMN "+def)), nil
		},
		// Always a rebuild. SQLite's DROP COLUMN refuses a column any index,
		// key, check, foreign key or generated column names, and finding out
		// which is a second introspection; a rebuild has none of those limits.
		DropColumn: func(t *schema.Table, c *schema.Column) string {
			return rebuildMark(t, alter(t, "DROP COLUMN "+q(c.Name)))
		},

		AlterColumnType: func(t *schema.Table, c *schema.Column) (string, error) {
			ty, err := liteddl.TypeSQL(t.Name, c)
			if c.Type.Enum {
				ty, err = "TEXT", nil
			}
			if err != nil {
				return "", err
			}
			return rebuildMark(t, alter(t, "ALTER COLUMN "+q(c.Name)+" TYPE "+ty)), nil
		},
		SetNotNull: func(t *schema.Table, c *schema.Column) (string, error) {
			return rebuildMark(t, alter(t, "ALTER COLUMN "+q(c.Name)+" SET NOT NULL")), nil
		},
		DropNotNull: func(t *schema.Table, c *schema.Column) (string, error) {
			return rebuildMark(t, alter(t, "ALTER COLUMN "+q(c.Name)+" DROP NOT NULL")), nil
		},
		SetDefault: func(t *schema.Table, c *schema.Column) string {
			return rebuildMark(t, alter(t, "ALTER COLUMN "+q(c.Name)+" SET DEFAULT "+liteddl.Default(c)))
		},
		DropDefault: func(t *schema.Table, c *schema.Column) string {
			return rebuildMark(t, alter(t, "ALTER COLUMN "+q(c.Name)+" DROP DEFAULT"))
		},
		FillNulls: func(t *schema.Table, c *schema.Column) string { return fillNulls(q, t, c) },

		AddUnique: func(t *schema.Table, u *schema.Unique) string {
			return rebuildMark(t, alter(t, "ADD CONSTRAINT "+q(u.Name)+" UNIQUE ("+identList(q, u.Columns)+")"))
		},
		AddCheck: func(t *schema.Table, c *schema.Check) string {
			return rebuildMark(t, alter(t, "ADD CONSTRAINT "+q(c.Name)+" CHECK ("+liteddl.CheckExpr(c)+")"))
		},
		DropConstraint: func(t *schema.Table, name string) string {
			return rebuildMark(t, alter(t, "DROP CONSTRAINT "+q(name)))
		},

		CreateIndex: func(t *schema.Table, ix *schema.Index) string {
			return inPlaceMark(t, liteddl.CreateIndex(t, ix))
		},
		DropIndex: func(t *schema.Table, ix *schema.Index) string {
			return inPlaceMark(t, liteddl.DropIndex(t, ix))
		},

		RenameTable: func(old, new string) string {
			return "ALTER TABLE " + q(old) + " RENAME TO " + q(new)
		},
		RenameColumn: func(t *schema.Table, old, new string) string {
			return alter(t, "RENAME COLUMN "+q(old)+" TO "+q(new))
		},

		AddPrimaryKey: func(t *schema.Table) string {
			return rebuildMark(t, alter(t, "ADD PRIMARY KEY ("+identList(q, t.PrimaryKey)+")"))
		},
		DropPrimaryKey: func(t *schema.Table) string { return rebuildMark(t, alter(t, "DROP PRIMARY KEY")) },

		// A column that stops being generated keeps what it computed: the
		// rebuild copies the old column's values into the new, plain one.
		DropExpression: func(t *schema.Table, c *schema.Column) string {
			return rebuildMark(t, alter(t, "ALTER COLUMN "+q(c.Name)+" DROP EXPRESSION"))
		},
		AddIdentity: func(t *schema.Table, c *schema.Column) []string {
			return []string{rebuildMark(t, alter(t, "ALTER COLUMN "+q(c.Name)+" ADD IDENTITY"))}
		},
		DropIdentity: func(t *schema.Table, c *schema.Column) string {
			return rebuildMark(t, alter(t, "ALTER COLUMN "+q(c.Name)+" DROP IDENTITY"))
		},
		AddSerial: func(t *schema.Table, c *schema.Column) []string {
			return []string{rebuildMark(t, alter(t, "ALTER COLUMN "+q(c.Name)+" ADD IDENTITY"))}
		},
		DropSerial: func(t *schema.Table, c *schema.Column) []string {
			return []string{rebuildMark(t, alter(t, "ALTER COLUMN "+q(c.Name)+" DROP IDENTITY"))}
		},
		// No concurrent index build, no NOT VALID, no comments: SQLite has
		// none of them, and a writer is serialised against every other here
		// anyway.
	}
	d.Enums = func(_ *Plan, from, to *schema.Schema) error {
		// No enum type: liteddl renders the labels as a CHECK named
		// ck_<table>_<column>, diffed as an ordinary check, as on Oracle.
		if len(from.Enums) == 0 && len(to.Enums) == 0 {
			return nil
		}
		return errors.New("migrate: schema still declares enums, which SQLite has no type " +
			"for: normalise the model with NormalizeSQLite (or storm diff against a live " +
			"database) so the labels are compared as the CHECK constraints it stores")
	}
	return d
}

// rebuildTables is the second pass: every table a marker asks to rebuild is
// rebuilt once, and every other marker becomes its statement. old is the
// database's schema once the renames have run, to the model's.
func rebuildTables(p Plan, old, to *schema.Schema) (Plan, error) {
	enums := enumsOf(to)
	need := map[string]bool{}
	last := map[string]int{}
	for i, c := range p.Changes {
		kind, table, _, ok := parseMark(c.SQL)
		if !ok {
			continue
		}
		last[table] = i
		// A marker on a table the plan drops — a foreign key between two
		// tables that both go — has nothing left to rebuild.
		if kind == "rebuild" && to.Table(table) != nil && old.Table(table) != nil {
			need[table] = true
		}
	}

	out := p
	out.Changes = make([]Change, 0, len(p.Changes))
	carried := map[string][]Change{}
	for i, c := range p.Changes {
		kind, table, body, ok := parseMark(c.SQL)
		if !ok {
			out.Changes = append(out.Changes, terminated(c))
			continue
		}
		switch {
		case need[table]:
			if c.Destructive {
				// The step, as a comment, so the guard reads it.
				carried[table] = append(carried[table], Change{
					SQL:         "-- by rebuilding " + table + ": " + firstLine(body),
					Destructive: true, Why: c.Why, Object: c.Object,
				})
			}
		case kind == "inplace":
			c.SQL = body
			out.Changes = append(out.Changes, terminated(c))
			continue
		default:
			continue // a rebuild of a table that is going anyway
		}
		if last[table] != i {
			continue
		}
		stmts, err := liteddl.Rebuild(old.Table(table), to.Table(table), enums)
		if err != nil {
			return Plan{}, err
		}
		out.Changes = append(out.Changes, carried[table]...)
		out.Changes = append(out.Changes, Change{
			SQL:    strings.Join(stmts, ";\n") + ";",
			Object: ObjectTable,
		})
	}
	return out, nil
}

// terminated ends a statement with the semicolon a migration file needs. A
// comment is left alone: it ends at its newline.
func terminated(c Change) Change {
	s := strings.TrimRight(c.SQL, " \n")
	if s != "" && !strings.HasPrefix(s, "--") && !strings.HasSuffix(s, ";") {
		c.SQL = s + ";"
	}
	return c
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// sqliteFile wraps a plan's SQL in what a rebuild needs around it: foreign key
// enforcement off, and every key checked before it is turned back on. A
// runner that wraps the file in a transaction makes the first PRAGMA a no-op,
// and the file says so, since a rebuild under enforcement cascades.
func sqliteFile(body string) string {
	return "-- storm:sqlite run this OUTSIDE a transaction, as one session: PRAGMA foreign_keys is\n" +
		"-- a no-op inside one, and a table rebuilt with enforcement on deletes its children's rows.\n" +
		"PRAGMA foreign_keys = OFF;\nBEGIN IMMEDIATE;\n" + body +
		"PRAGMA foreign_key_check;\nCOMMIT;\nPRAGMA foreign_keys = ON;\n"
}

// normalizeSQLite serialises use of the scratch database name on one
// connection, for the reason the other back ends' mutexes exist.
var normalizeSQLite sync.Mutex

// sqliteScratch is the name the scratch database is attached under.
const sqliteScratch = "storm_normalize"

// NormalizeSQLite renders a schema as DDL, applies it to an in-memory database
// attached beside the connection's own, reads it back, and detaches it.
//
// The result is the model as SQLite stores it — an enum as TEXT and a CHECK,
// a uuid key's default gone, a now() as the clock expression — which is the
// form a live database is compared in.
//
// c must be ONE connection: an attached database belongs to the connection
// that attached it. Wrap a *sql.Conn with sqlitedrv.New, or use a pool limited
// to one connection. It must not be inside a transaction, which ATTACH refuses.
func NormalizeSQLite(ctx context.Context, c Conn, s *schema.Schema) (*schema.Schema, error) {
	normalizeSQLite.Lock()
	defer normalizeSQLite.Unlock()

	stmts, err := liteddl.StatementsIn(s, sqliteScratch)
	if err != nil {
		return nil, err
	}
	// A leftover from a call that could not detach, on this connection.
	_, _ = c.Exec(ctx, "DETACH DATABASE "+sqliteScratch, nil)
	if _, err := c.Exec(ctx, "ATTACH DATABASE ':memory:' AS "+sqliteScratch, nil); err != nil {
		return nil, fmt.Errorf("attach the scratch database: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_, _ = c.Exec(ctx, "DETACH DATABASE "+sqliteScratch, nil)
	}()
	for _, st := range stmts {
		if _, err := c.Exec(ctx, st, nil); err != nil {
			return nil, fmt.Errorf("apply model DDL to the scratch database: %w\n  statement: %s", err, st)
		}
	}
	norm, err := schemasqlite.Introspect(ctx, c, sqliteScratch)
	if err != nil {
		return nil, err
	}
	carryConversions(norm, s)
	return norm, nil
}

// ForSQLite is the plan that brings the connection's main database to want. c
// must be one connection; see NormalizeSQLite.
func ForSQLite(ctx context.Context, c Conn, want *schema.Schema, o Options) (Plan, error) {
	if o.Concurrently {
		return Plan{}, errors.New("migrate: Options.Concurrently has no SQLite form: " +
			"there is no non-blocking index build, and writers are serialised anyway")
	}
	norm, err := NormalizeSQLite(ctx, c, want)
	if err != nil {
		return Plan{}, err
	}
	return diffSQLite(ctx, c, norm, o)
}

func diffSQLite(ctx context.Context, c Conn, norm *schema.Schema, o Options) (Plan, error) {
	cur, err := schemasqlite.Introspect(ctx, c, "")
	if err != nil {
		return Plan{}, fmt.Errorf("introspect: %w", err)
	}
	if o.DeclaredOnly {
		cur = declaredOnly(cur, norm)
	}
	return DiffFor(cur, norm, SQLite)
}

// AutoSQLiteDB is AutoSQLite on one connection taken from db, and given back.
func AutoSQLiteDB(ctx context.Context, db *sql.DB, want *schema.Schema, o AutoOptions) (Result, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("take a connection to migrate on: %w", err)
	}
	defer conn.Close()
	return AutoSQLite(ctx, sqlitedrv.New(conn), want, o)
}

// AutoSQLite brings the connection's database up to date with want, and
// reports what it applied. Same promises as Auto's, kept by SQLite's means:
//
//	the lock          BEGIN IMMEDIATE: the database's one write lock, taken
//	                  before the plan is computed, so a second process waits
//	                  and then finds nothing left to do
//	the step bound    the connection's busy_timeout; LockWait bounds the
//	                  wait for the lock itself
//	all or nothing    one transaction — SQLite's DDL is transactional — and
//	                  PRAGMA foreign_key_check before it commits
//
// Foreign key enforcement is turned off for the call, because a table rebuild
// under enforcement deletes its children's rows (see liteddl.Rebuild), and back
// to what it was afterwards. A migration that leaves a key dangling is rolled
// back and reported, not committed.
//
// c must be ONE connection, not inside a transaction; AutoSQLiteDB takes one
// from a *sql.DB.
func AutoSQLite(ctx context.Context, c Conn, want *schema.Schema, o AutoOptions) (Result, error) {
	if o.Concurrently {
		return Result{}, errors.New("migrate: AutoOptions.Concurrently has no SQLite form")
	}
	if err := liteddl.Check(want); err != nil {
		return Result{}, err
	}
	// Before the transaction: ATTACH, which normalisation needs, cannot run
	// inside one. The model's normal form does not depend on the database.
	norm, err := NormalizeSQLite(ctx, c, want)
	if err != nil {
		return Result{}, err
	}

	fkWas, err := pragmaInt(ctx, c, "PRAGMA foreign_keys")
	if err != nil {
		return Result{}, err
	}
	if _, err := c.Exec(ctx, "PRAGMA foreign_keys = OFF", nil); err != nil {
		return Result{}, err
	}
	defer func() {
		if fkWas != 0 {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			_, _ = c.Exec(ctx, "PRAGMA foreign_keys = ON", nil)
		}
	}()

	waited, err := beginImmediate(ctx, c, o)
	res := Result{Waited: waited}
	if err != nil {
		return res, err
	}
	committed := false
	defer func() {
		if !committed {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			_, _ = c.Exec(ctx, "ROLLBACK", nil)
		}
	}()

	plan, err := diffSQLite(ctx, c, norm, Options{DeclaredOnly: o.DeclaredOnly})
	if err != nil {
		return res, err
	}
	if plan.Empty() {
		o.logf("storm: the database already matches the model")
		return res, nil
	}
	if refused := (&DestructiveError{Plan: plan, accept: o.Accept}); !o.AllowDestructive && len(refused.Destructive()) > 0 {
		return res, refused
	}
	for _, ch := range plan.Changes {
		if strings.HasPrefix(ch.SQL, "--") && !strings.Contains(ch.SQL, "\n") {
			res.Applied = append(res.Applied, ch) // a destructive step's note; its rebuild follows
			continue
		}
		if _, err := c.Exec(ctx, ch.SQL, nil); err != nil {
			return Result{Waited: waited}, fmt.Errorf("%w\n  while applying: %s\n  nothing was applied: the transaction rolled back", err, ch.SQL)
		}
		o.logf("storm: %s", ch.SQL)
		res.Applied = append(res.Applied, ch)
	}
	if err := foreignKeyCheck(ctx, c); err != nil {
		return Result{Waited: waited}, err
	}
	if _, err := c.Exec(ctx, "COMMIT", nil); err != nil {
		return Result{Waited: waited}, fmt.Errorf("commit migration: %w", err)
	}
	committed = true
	o.logf("storm: applied %d step(s)", len(res.Applied))
	return res, nil
}

// beginImmediate takes the write lock, polling while another connection holds
// it, for at most LockWait.
func beginImmediate(ctx context.Context, c Conn, o AutoOptions) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(o.lockWait())
	for attempt := 0; ; attempt++ {
		_, err := c.Exec(ctx, "BEGIN IMMEDIATE", nil)
		if err == nil {
			return time.Since(start), nil
		}
		if !isBusy(err) {
			return time.Since(start), fmt.Errorf("begin migration: %w", err)
		}
		if !time.Now().Before(deadline) {
			return time.Since(start), fmt.Errorf("%w: waited %s", ErrLockBusy, time.Since(start).Round(time.Millisecond))
		}
		if attempt == 0 {
			o.logf("storm: another connection is writing; waiting up to %s", o.lockWait())
		}
		select {
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// isBusy recognises SQLITE_BUSY by its message: storm imports no driver, so
// it has no error type to match, and every driver reports the engine's text.
func isBusy(err error) bool {
	s := err.Error()
	return strings.Contains(s, "database is locked") || strings.Contains(s, "SQLITE_BUSY")
}

// foreignKeyCheck fails when any row's key points at nothing.
func foreignKeyCheck(ctx context.Context, c Conn) error {
	rows, err := runtime.AsValueRows(c.Query(ctx, "PRAGMA foreign_key_check", nil))
	if err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}
	defer rows.Close()
	var bad []string
	for rows.Next() && len(bad) < 5 {
		v := rows.Values()
		if len(v) >= 3 {
			bad = append(bad, valdec.Str(v[0])+" row "+valdec.Str(v[1])+" → "+valdec.Str(v[2]))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("migrate: after the migration a foreign key points at nothing (%s); "+
			"nothing was applied: the transaction rolled back", strings.Join(bad, "; "))
	}
	return nil
}

func pragmaInt(ctx context.Context, c Conn, q string) (int64, error) {
	rows, err := runtime.AsValueRows(c.Query(ctx, q, nil))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", q, err)
	}
	defer rows.Close()
	var n int64
	if rows.Next() {
		if v := rows.Values(); len(v) > 0 {
			n = valdec.Int8(v[0])
		}
	}
	return n, rows.Err()
}
