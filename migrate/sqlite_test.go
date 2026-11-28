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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm/v2/runtime"
	"github.com/gsoultan/storm/v2/schema"
)

// SQLite's half of the differ and of AutoSQLite, without a database.
//
// The live half — every kind of change applied to a real file, rows checked
// to survive, the CLI end to end — is internal/sqlitecheck, a module of its
// own because it imports the driver and this module imports none. What is
// here is what the plan SAYS, and the order AutoSQLite says it in, against a
// connection that answers the catalogue queries from a script.

func TestSQLitePlanMergesATablesChangesIntoOneRebuild(t *testing.T) {
	from := lsch(ltbl("people", lcol("id", "int8", true), lcol("email", "text", false), lcol("nick", "text", false)))
	to := lsch(ltbl("people", lcol("id", "int8", true), lcol("email", "text", true)))
	to.Tables[0].Checks = []*schema.Check{{Name: "ck_people_email", Expr: "email <> ''"}}

	p, err := DiffFor(from, to, SQLite)
	if err != nil {
		t.Fatal(err)
	}
	var rebuilds, notes int
	for _, c := range p.Changes {
		switch {
		case strings.Contains(c.SQL, "_storm_rebuild_people"):
			rebuilds++
		case strings.HasPrefix(c.SQL, "-- by rebuilding people: "):
			notes++
			if !c.Destructive || c.Why == "" {
				t.Errorf("the carried step lost its guard: %+v", c)
			}
		}
		if strings.HasPrefix(c.SQL, "\x00") {
			t.Errorf("a marker reached the plan: %q", c.SQL)
		}
	}
	if rebuilds != 1 {
		t.Errorf("want ONE rebuild for three changes to one table, got %d:\n%s", rebuilds, p.SQL())
	}
	if notes == 0 {
		t.Errorf("the dropped column is not in the plan as a destructive step:\n%s", p.SQL())
	}
	sql := p.SQL()
	for _, want := range []string{"PRAGMA foreign_keys = OFF;", "BEGIN IMMEDIATE;", "PRAGMA foreign_key_check;", "COMMIT;"} {
		if !strings.Contains(sql, want) {
			t.Errorf("the file is missing %q:\n%s", want, sql)
		}
	}
	if sub := p.Subset(p.Changes[:1]); !strings.Contains(sub.SQL(), "BEGIN IMMEDIATE;") {
		t.Errorf("a subset forgot its dialect:\n%s", sub.SQL())
	}
}

func TestSQLiteAddsAColumnAndAnIndexInPlace(t *testing.T) {
	from := lsch(ltbl("people", lcol("id", "int8", true)))
	to := lsch(ltbl("people", lcol("id", "int8", true), lcol("nick", "text", false)))
	to.Tables[0].Indexes = []*schema.Index{{Name: "ix_people_nick", Columns: []schema.IndexColumn{{Name: "nick"}}}}

	p, err := DiffFor(from, to, SQLite)
	if err != nil {
		t.Fatal(err)
	}
	sql := p.SQL()
	if strings.Contains(sql, "_storm_rebuild_") {
		t.Errorf("an addable column rebuilt the table:\n%s", sql)
	}
	for _, want := range []string{`ALTER TABLE "people" ADD COLUMN "nick"`, `CREATE INDEX "ix_people_nick"`} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q:\n%s", want, sql)
		}
	}
	for _, c := range p.Changes {
		if !strings.HasSuffix(c.SQL, ";") {
			t.Errorf("unterminated statement %q", c.SQL)
		}
	}
}

// A NOT NULL column with no default cannot be added; the table is rebuilt.
func TestSQLiteRebuildsForAColumnItCannotAdd(t *testing.T) {
	from := lsch(ltbl("people", lcol("id", "int8", true)))
	c := lcol("tier", "int4", true)
	c.Default = "now()"
	to := lsch(ltbl("people", lcol("id", "int8", true), c))
	p, err := DiffFor(from, to, SQLite)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.SQL(), "_storm_rebuild_people") {
		t.Errorf("want a rebuild:\n%s", p.SQL())
	}
}

// Two linked tables dropped together: the foreign key's marker names a table
// that is going, and must not rebuild it.
func TestSQLiteDropsLinkedTablesWithoutRebuildingThem(t *testing.T) {
	parent := ltbl("orgs", lcol("id", "int8", true))
	child := ltbl("members", lcol("id", "int8", true), lcol("org_id", "int8", false))
	child.ForeignKeys = []*schema.ForeignKey{{Name: "fk_members_org_id", Columns: []string{"org_id"},
		RefTable: "orgs", RefColumns: []string{"id"}}}
	p, err := DiffFor(lsch(parent, child), lsch(), SQLite)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.SQL(), "_storm_rebuild_") {
		t.Errorf("a dropped table was rebuilt:\n%s", p.SQL())
	}

	// And a foreign key added to a kept table, to a new one, is a rebuild.
	p, err = DiffFor(lsch(ltbl("members", lcol("id", "int8", true), lcol("org_id", "int8", false))),
		lsch(parent, child), SQLite)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.SQL(), "_storm_rebuild_members") {
		t.Errorf("a new foreign key did not rebuild its table:\n%s", p.SQL())
	}
}

// Every renderer with no ALTER answers with a rebuild. Each change below is
// one the differ asks for, and each must come out as the one rebuild.
func TestSQLiteEveryChangeWithoutAnAlterIsARebuild(t *testing.T) {
	base := func() *schema.Table {
		return ltbl("t", lcol("id", "int8", true), lcol("a", "int4", false), lcol("b", "text", true))
	}
	cases := map[string]func(*schema.Table){
		"retype":         func(t *schema.Table) { t.Column("a").Type = schema.Type{Name: schema.TypeInt8} },
		"enum retype":    func(t *schema.Table) { t.Column("a").Type = schema.Type{Name: "mood", Enum: true} },
		"set not null":   func(t *schema.Table) { t.Column("a").NotNull = true },
		"drop not null":  func(t *schema.Table) { t.Column("b").NotNull = false },
		"set default":    func(t *schema.Table) { t.Column("a").Default = "7" },
		"add unique":     func(t *schema.Table) { t.Uniques = []*schema.Unique{{Name: "uq_t_b", Columns: []string{"b"}}} },
		"primary key":    func(t *schema.Table) { t.PrimaryKey = []string{"id", "a"}; t.Column("a").NotNull = true },
		"add identity":   func(t *schema.Table) { t.Column("id").Identity = true },
		"add generated":  func(t *schema.Table) { t.Column("a").Generated = "length(b)" },
		"add serial":     func(t *schema.Table) { t.Column("id").Serial = true },
		"drop the check": func(t *schema.Table) {},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			from, to := base(), base()
			if name == "drop the check" {
				from.Checks = []*schema.Check{{Name: "ck_t_a", Expr: "a > 0"}}
			}
			if name == "drop not null" || name == "set default" {
				from.Column("a").Default = ""
			}
			change(to)
			old, want := lsch(from), lsch(to)
			if name == "enum retype" {
				// The differ sees enums only on a model nobody normalised.
				p, err := DiffFor(old, want, SQLite)
				if err == nil && !strings.Contains(p.SQL(), "_storm_rebuild_t") {
					t.Errorf("want a rebuild or a refusal:\n%s", p.SQL())
				}
				return
			}
			p, err := DiffFor(old, want, SQLite)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(p.SQL(), "_storm_rebuild_t") {
				t.Errorf("want a rebuild:\n%s", p.SQL())
			}
		})
	}
	// And the reverse of the ones that have one.
	for name, change := range map[string]func(*schema.Table){
		"drop identity":  func(t *schema.Table) { t.Column("id").Identity = true },
		"drop generated": func(t *schema.Table) { t.Column("a").Generated = "length(b)" },
		"drop default":   func(t *schema.Table) { t.Column("a").Default = "7" },
		"drop key":       func(t *schema.Table) { t.PrimaryKey = []string{"id", "a"}; t.Column("a").NotNull = true },
		"drop serial":    func(t *schema.Table) { t.Column("id").Serial = true },
	} {
		t.Run(name, func(t *testing.T) {
			from, to := base(), base()
			change(from)
			p, err := DiffFor(lsch(from), lsch(to), SQLite)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(p.SQL(), "_storm_rebuild_t") {
				t.Errorf("want a rebuild:\n%s", p.SQL())
			}
		})
	}
}

// Renames have an ALTER, and stay statements of their own.
func TestSQLiteRenamesAreAlters(t *testing.T) {
	from := lsch(ltbl("people", lcol("id", "int8", true), lcol("nick", "text", false)))
	to := lsch(ltbl("persons", lcol("id", "int8", true), lcol("handle", "text", false)))
	to.Tables[0].RenamedFrom = "people"
	to.Tables[0].Column("handle").RenamedFrom = "nick"
	p, err := DiffFor(from, to, SQLite)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`ALTER TABLE "people" RENAME TO "persons";`, `RENAME COLUMN "nick" TO "handle";`} {
		if !strings.Contains(p.SQL(), want) {
			t.Errorf("missing %q:\n%s", want, p.SQL())
		}
	}
}

func TestSQLiteRefusesAnUnnormalisedEnum(t *testing.T) {
	to := lsch(ltbl("t", lcol("id", "int8", true)))
	to.Enums = []*schema.Enum{{Name: "mood", Labels: []string{"ok"}}}
	if _, err := DiffFor(lsch(), to, SQLite); err == nil || !strings.Contains(err.Error(), "NormalizeSQLite") {
		t.Errorf("want the enum refused with a pointer to NormalizeSQLite, got %v", err)
	}
}

func TestSQLiteMarkersAndHelpers(t *testing.T) {
	tb := &schema.Table{Name: "t"}
	if k, tab, body, ok := parseMark(rebuildMark(tb, "x")); !ok || k != "rebuild" || tab != "t" || body != "x" {
		t.Errorf("rebuild marker: %q %q %q %v", k, tab, body, ok)
	}
	if k, _, body, ok := parseMark(inPlaceMark(tb, "CREATE INDEX i")); !ok || k != "inplace" || body != "CREATE INDEX i" {
		t.Errorf("in-place marker: %q %q %v", k, body, ok)
	}
	for _, s := range []string{"CREATE TABLE t", markRebuild + "no-separator"} {
		if _, _, _, ok := parseMark(s); ok {
			t.Errorf("%q parsed as a marker", s)
		}
	}
	if got := terminated(Change{SQL: "-- a note"}).SQL; got != "-- a note" {
		t.Errorf("a comment was terminated: %q", got)
	}
	if got := terminated(Change{SQL: "DROP TABLE t;\n"}).SQL; got != "DROP TABLE t;\n" {
		t.Errorf("a terminated statement changed: %q", got)
	}
	if got := terminated(Change{SQL: "DROP TABLE t\n"}).SQL; got != "DROP TABLE t;" {
		t.Errorf("got %q", got)
	}
	if firstLine("a\nb") != "a" || firstLine("a") != "a" {
		t.Error("firstLine")
	}
	if !isBusy(errors.New("database is locked (5) (SQLITE_BUSY)")) || isBusy(errors.New("no such table")) {
		t.Error("isBusy")
	}
}

func ltbl(name string, cols ...*schema.Column) *schema.Table {
	return &schema.Table{Name: name, Columns: cols, PrimaryKey: []string{"id"}}
}

func lcol(name, typ string, notNull bool) *schema.Column {
	return &schema.Column{Name: name, Type: schema.Type{Name: typ}, NotNull: notNull}
}

func lsch(ts ...*schema.Table) *schema.Schema { return &schema.Schema{Tables: ts} }

// ---- AutoSQLite against a scripted connection --------------------------------

// liteTable is one table as the catalogue queries report it.
type liteTable struct {
	name, sql string
	cols      [][]any // name, type, notnull, dflt_value, pk, hidden
}

type liteFake struct {
	db map[string][]liteTable // by attached database: "main", "storm_normalize"

	fk       int64
	busy     int // BEGIN IMMEDIATE reports busy this many times
	dangling [][]any
	failExec map[string]error // by statement prefix
	failQry  map[string]error // by query substring
	execs    []string
}

func (f *liteFake) Exec(_ context.Context, sql string, _ []any) (int64, error) {
	f.execs = append(f.execs, sql)
	for p, err := range f.failExec {
		if strings.HasPrefix(sql, p) {
			return 0, err
		}
	}
	if sql == "BEGIN IMMEDIATE" && f.busy > 0 {
		f.busy--
		return 0, errors.New("database is locked")
	}
	return 0, nil
}

func (f *liteFake) Query(_ context.Context, sql string, args []any) (runtime.Rows, error) {
	for s, err := range f.failQry {
		if strings.Contains(sql, s) {
			return nil, err
		}
	}
	switch {
	case strings.Contains(sql, ".sqlite_schema"):
		from := sql[strings.Index(sql, "FROM ")+5 : strings.Index(sql, ".sqlite_schema")]
		var rows [][]any
		for _, t := range f.db[strings.Trim(from, `"`)] {
			rows = append(rows, []any{t.name, t.sql})
		}
		return &liteRows{rows: rows}, nil
	case strings.Contains(sql, "pragma_table_xinfo"):
		for _, t := range f.db[args[1].(string)] {
			if t.name == args[0] {
				return &liteRows{rows: t.cols}, nil
			}
		}
	case sql == "PRAGMA foreign_keys":
		return &liteRows{rows: [][]any{{f.fk}}}, nil
	case sql == "PRAGMA foreign_key_check":
		return &liteRows{rows: f.dangling}, nil
	}
	return &liteRows{}, nil
}

func (f *liteFake) ran(sql string) bool {
	for _, e := range f.execs {
		if strings.HasPrefix(e, sql) {
			return true
		}
	}
	return false
}

type liteRows struct {
	rows [][]any
	i    int
}

func (r *liteRows) Next() bool          { r.i++; return r.i <= len(r.rows) }
func (r *liteRows) Values() []any       { return r.rows[r.i-1] }
func (r *liteRows) RawValues() [][]byte { return nil }
func (r *liteRows) Close()              {}
func (r *liteRows) Err() error          { return nil }

// notes(id, body) in main; the model, as normalisation reads it back, is
// notes(id) — so the plan drops a column, which is a rebuild and destructive.
func notesFake() *liteFake {
	id := []any{"id", "INTEGER", int64(1), nil, int64(1), int64(0)}
	body := []any{"body", "TEXT", int64(0), nil, int64(0), int64(0)}
	return &liteFake{fk: 1, db: map[string][]liteTable{
		"main":        {{name: "notes", sql: `CREATE TABLE "notes" ("id" INTEGER NOT NULL, "body" TEXT, PRIMARY KEY ("id"))`, cols: [][]any{id, body}}},
		sqliteScratch: {{name: "notes", sql: `CREATE TABLE "notes" ("id" INTEGER NOT NULL, PRIMARY KEY ("id"))`, cols: [][]any{id}}},
	}}
}

func notesModel() *schema.Schema { return lsch(ltbl("notes", lcol("id", "int4", true))) }

func TestAutoSQLiteRefusesADestructivePlanAndRollsBack(t *testing.T) {
	f := notesFake()
	_, err := AutoSQLite(context.Background(), f, notesModel(), AutoOptions{})
	var de *DestructiveError
	if !errors.As(err, &de) {
		t.Fatalf("want a DestructiveError, got %v", err)
	}
	if !f.ran("ROLLBACK") || f.ran("COMMIT") {
		t.Errorf("a refused plan must roll back: %q", f.execs)
	}
	if !f.ran("PRAGMA foreign_keys = ON") {
		t.Errorf("enforcement was not turned back on: %q", f.execs)
	}
	if !f.ran("DETACH DATABASE") {
		t.Errorf("the scratch database was not detached: %q", f.execs)
	}
}

func TestAutoSQLiteAppliesARebuild(t *testing.T) {
	f := notesFake()
	var logs []string
	res, err := AutoSQLite(context.Background(), f, notesModel(), AutoOptions{
		AllowDestructive: true,
		Logf:             func(format string, a ...any) { logs = append(logs, format) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) < 2 || !strings.HasPrefix(res.Applied[0].SQL, "-- by rebuilding notes") {
		t.Errorf("applied: %+v", res.Applied)
	}
	order := []string{"PRAGMA foreign_keys = OFF", "BEGIN IMMEDIATE", "CREATE TABLE \"_storm_rebuild_notes\"", "COMMIT", "PRAGMA foreign_keys = ON"}
	at := 0
	for _, e := range f.execs {
		if at < len(order) && strings.HasPrefix(e, order[at]) {
			at++
		}
	}
	if at != len(order) {
		t.Errorf("statements out of order, stopped before %q:\n%s", order[at], strings.Join(f.execs, "\n"))
	}
	for _, e := range f.execs {
		if strings.HasPrefix(e, "-- ") {
			t.Errorf("a note was sent to the database: %q", e)
		}
	}
	if len(logs) == 0 {
		t.Error("nothing was logged")
	}
}

func TestAutoSQLiteWithNothingToDo(t *testing.T) {
	f := notesFake()
	f.db["main"] = f.db[sqliteScratch]
	f.fk = 0
	res, err := AutoSQLite(context.Background(), f, notesModel(), AutoOptions{})
	if err != nil || !res.Empty() {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if f.ran("PRAGMA foreign_keys = ON") {
		t.Error("enforcement that was off was turned on")
	}
}

func TestAutoSQLiteRollsBackADanglingKey(t *testing.T) {
	f := notesFake()
	f.dangling = [][]any{{"members", int64(3), "orgs", int64(0)}}
	_, err := AutoSQLite(context.Background(), f, notesModel(), AutoOptions{AllowDestructive: true})
	if err == nil || !strings.Contains(err.Error(), "members row 3 → orgs") {
		t.Fatalf("want the dangling row named, got %v", err)
	}
	if f.ran("COMMIT") || !f.ran("ROLLBACK") {
		t.Errorf("%q", f.execs)
	}
}

func TestAutoSQLiteWaitsForTheWriteLock(t *testing.T) {
	f := notesFake()
	f.db["main"] = f.db[sqliteScratch]
	f.busy = 1
	res, err := AutoSQLite(context.Background(), f, notesModel(), AutoOptions{LockWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Waited <= 0 {
		t.Error("the wait was not reported")
	}

	f.busy = 1 << 30
	_, err = AutoSQLite(context.Background(), f, notesModel(), AutoOptions{LockWait: time.Millisecond})
	if !errors.Is(err, ErrLockBusy) {
		t.Errorf("want ErrLockBusy, got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := beginImmediate(ctx, f, AutoOptions{LockWait: time.Hour}); !errors.Is(err, context.Canceled) {
		t.Errorf("want the context's error, got %v", err)
	}
}

func TestAutoSQLiteReportsEachFailure(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name string
		set  func(*liteFake)
		want string
	}{
		{"attach", func(f *liteFake) { f.failExec = map[string]error{"ATTACH": boom} }, "attach the scratch database"},
		{"model DDL", func(f *liteFake) { f.failExec = map[string]error{"CREATE TABLE": boom} }, "apply model DDL"},
		{"scratch introspection", func(f *liteFake) { f.failQry = map[string]error{`"storm_normalize".sqlite_schema`: boom} }, "boom"},
		{"read foreign_keys", func(f *liteFake) { f.failQry = map[string]error{"PRAGMA foreign_keys": boom} }, "PRAGMA foreign_keys"},
		{"foreign_keys off", func(f *liteFake) { f.failExec = map[string]error{"PRAGMA foreign_keys = OFF": boom} }, "boom"},
		{"begin", func(f *liteFake) { f.failExec = map[string]error{"BEGIN": boom} }, "begin migration"},
		{"introspection", func(f *liteFake) { f.failQry = map[string]error{`"main".sqlite_schema`: boom} }, "introspect"},
		{"apply", func(f *liteFake) { f.failExec = map[string]error{"CREATE TABLE \"_storm_rebuild_": boom} }, "nothing was applied"},
		{"foreign key check", func(f *liteFake) { f.failQry = map[string]error{"foreign_key_check": boom} }, "check foreign keys"},
		{"commit", func(f *liteFake) { f.failExec = map[string]error{"COMMIT": boom} }, "commit migration"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := notesFake()
			c.set(f)
			_, err := AutoSQLite(context.Background(), f, notesModel(), AutoOptions{AllowDestructive: true})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestSQLiteRefusesWhatItHasNoFormFor(t *testing.T) {
	ctx := context.Background()
	if _, err := AutoSQLite(ctx, notesFake(), notesModel(), AutoOptions{Concurrently: true}); err == nil {
		t.Error("AutoOptions.Concurrently was accepted")
	}
	if _, err := ForSQLite(ctx, notesFake(), notesModel(), Options{Concurrently: true}); err == nil {
		t.Error("Options.Concurrently was accepted")
	}
	// A model liteddl cannot render is refused before anything runs.
	bad := lsch(&schema.Table{Name: "t", Columns: []*schema.Column{{Name: "x", Type: schema.Type{Name: "tsvector"}}}})
	f := notesFake()
	if _, err := AutoSQLite(ctx, f, bad, AutoOptions{}); err == nil {
		t.Error("an unportable model was accepted")
	}
	if len(f.execs) != 0 {
		t.Errorf("statements ran for a refused model: %q", f.execs)
	}
	if _, err := NormalizeSQLite(ctx, f, bad); err == nil {
		t.Error("NormalizeSQLite accepted an unportable model")
	}
}

func TestForSQLitePlansAgainstTheLiveDatabase(t *testing.T) {
	p, err := ForSQLite(context.Background(), notesFake(), notesModel(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.SQL(), "_storm_rebuild_notes") {
		t.Errorf("want the rebuild:\n%s", p.SQL())
	}
	// DeclaredOnly leaves a table the model does not name alone.
	f := notesFake()
	f.db["main"] = append(f.db["main"], liteTable{name: "other", sql: `CREATE TABLE "other" ("id" INTEGER)`,
		cols: [][]any{{"id", "INTEGER", int64(0), nil, int64(0), int64(0)}}})
	p, err = ForSQLite(context.Background(), f, notesModel(), Options{DeclaredOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.SQL(), `DROP TABLE "other"`) {
		t.Errorf("DeclaredOnly dropped an undeclared table:\n%s", p.SQL())
	}
}
