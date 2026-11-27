// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlitecheck

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/v2/migrate"
	"github.com/gsoultan/storm/v2/runtime/sqlitedrv"
	"github.com/gsoultan/storm/v2/schema"
)

// Migrations, applied. For every kind of schema change a model can make, a
// database at `from` is migrated to `to` by migrate.AutoSQLite, and then the
// differ must find NOTHING left to do — the property migrate/converge_test.go
// holds PostgreSQL to. On SQLite most of these are table REBUILDS, so each
// case that keeps data seeds rows first and checks they are still there.

func tbl(name string, cols ...*schema.Column) *schema.Table {
	return &schema.Table{Name: name, Columns: cols, PrimaryKey: []string{"id"}}
}

func col(name, typ string, notNull bool) *schema.Column {
	return &schema.Column{Name: name, Type: schema.Type{Name: typ}, NotNull: notNull}
}

func vcol(name string, size int) *schema.Column {
	return &schema.Column{Name: name, Type: schema.Type{Name: schema.TypeVarchar, Size: size}}
}

func sch(ts ...*schema.Table) *schema.Schema { return &schema.Schema{Tables: ts} }

func key(cols ...string) []schema.IndexColumn {
	out := make([]schema.IndexColumn, len(cols))
	for i, c := range cols {
		out[i] = schema.IndexColumn{Name: c}
	}
	return out
}

func orgs(uniqueName string) *schema.Table {
	t := tbl("orgs", col("id", "int8", true), col("code", "text", true))
	t.Uniques = []*schema.Unique{{Name: uniqueName, Columns: []string{"code"}}}
	return t
}

func members(fk *schema.ForeignKey) *schema.Table {
	t := tbl("members", col("id", "int8", true), col("org_code", "text", false))
	if fk != nil {
		t.ForeignKeys = []*schema.ForeignKey{fk}
	}
	return t
}

func memberFK() *schema.ForeignKey {
	return &schema.ForeignKey{Name: "fk_members_org_code", Columns: []string{"org_code"},
		RefTable: "orgs", RefColumns: []string{"code"}}
}

type migCase struct {
	name     string
	from, to func() *schema.Schema
	// seed runs against `from` and check against `to`: a query whose one
	// value must be 1.
	seed, check string
	// wants is a piece of the plan's SQL, so a case that stopped exercising
	// what it is named for fails rather than passing on an empty plan.
	wants string
}

var migCases = []migCase{
	{
		name: "drop a column with an index, a unique and a check on it",
		from: func() *schema.Schema {
			t := tbl("people", col("id", "int8", true), col("email", "text", true), vcol("nick", 40))
			t.Indexes = []*schema.Index{{Name: "ix_people_nick", Columns: key("nick")}}
			t.Uniques = []*schema.Unique{{Name: "uq_people_email_nick", Columns: []string{"email", "nick"}}}
			t.Checks = []*schema.Check{{Name: "ck_people_nick", Expr: "nick <> ''"}}
			return sch(t)
		},
		to:    func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), col("email", "text", true))) },
		seed:  `INSERT INTO people VALUES (1, 'a@x', 'ada')`,
		check: `SELECT count(*) = 1 FROM people WHERE email = 'a@x'`,
		wants: "_storm_rebuild_people",
	},
	{
		name: "add a column in place, with an index",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true))) },
		to: func() *schema.Schema {
			return sch(&schema.Table{Name: "people", PrimaryKey: []string{"id"},
				Columns: []*schema.Column{col("id", "int8", true), vcol("nick", 40)},
				Indexes: []*schema.Index{{Name: "ix_people_nick", Columns: key("nick")}}})
		},
		seed:  `INSERT INTO people VALUES (7)`,
		check: `SELECT count(*) = 1 FROM people WHERE id = 7 AND nick IS NULL`,
		wants: `ADD COLUMN "nick"`,
	},
	{
		name: "add a NOT NULL column with a constant default, in place",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true))) },
		to: func() *schema.Schema {
			c := col("tier", "int4", true)
			c.Default = "3"
			return sch(tbl("people", col("id", "int8", true), c))
		},
		seed:  `INSERT INTO people VALUES (1)`,
		check: `SELECT tier = 3 FROM people`,
		wants: `ADD COLUMN "tier"`,
	},
	{
		name: "add a column whose default is the clock, by rebuilding",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true))) },
		to: func() *schema.Schema {
			c := col("joined", schema.TypeTimestamptz, true)
			c.Default = "now()"
			return sch(tbl("people", col("id", "int8", true), c))
		},
		seed:  `INSERT INTO people VALUES (1)`,
		check: `SELECT joined LIKE '2___-__-__T__:__:__._________Z' FROM people`,
		wants: "_storm_rebuild_people",
	},
	{
		name: "retype, tighten and default a column, backfilling its NULLs",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), vcol("nick", 40))) },
		to: func() *schema.Schema {
			c := vcol("nick", 80)
			c.NotNull, c.Default, c.FillNulls = true, "'anon'", "'anon'"
			return sch(tbl("people", col("id", "int8", true), c))
		},
		seed:  `INSERT INTO people (id) VALUES (1); INSERT INTO people VALUES (2, 'bo')`,
		check: `SELECT count(*) = 2 FROM people WHERE (id = 1 AND nick = 'anon') OR (id = 2 AND nick = 'bo')`,
		wants: `UPDATE "people" SET "nick" = 'anon'`,
	},
	{
		name: "loosen a column and drop its default",
		from: func() *schema.Schema {
			c := col("nick", "text", true)
			c.Default = "'x'"
			return sch(tbl("people", col("id", "int8", true), c))
		},
		to:    func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), col("nick", "text", false))) },
		seed:  `INSERT INTO people (id) VALUES (1)`,
		check: `SELECT nick = 'x' FROM people`,
	},
	{
		name: "change, drop and add indexes",
		from: func() *schema.Schema {
			t := tbl("people", col("id", "int8", true), col("a", "text", false), col("b", "text", false))
			t.Indexes = []*schema.Index{
				{Name: "ix_people_a", Columns: key("a")},
				{Name: "ix_people_gone", Columns: key("b")},
				{Name: "ix_people_u", Columns: key("a"), Unique: true},
			}
			return sch(t)
		},
		to: func() *schema.Schema {
			t := tbl("people", col("id", "int8", true), col("a", "text", false), col("b", "text", false))
			t.Indexes = []*schema.Index{
				{Name: "ix_people_a", Columns: key("a", "b")},
				{Name: "ix_people_new", Columns: key("b"), Where: "b IS NOT NULL"},
				{Name: "ix_people_u", Columns: key("a")},
				{Name: "ix_people_lower", Columns: []schema.IndexColumn{{Name: "lower(b)", Expr: true, Desc: true}}},
			}
			return sch(t)
		},
		wants: "CREATE INDEX",
	},
	{
		name: "change the primary key",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), col("email", "text", true))) },
		to: func() *schema.Schema {
			t := tbl("people", col("id", "int8", true), col("email", "text", true))
			t.PrimaryKey = []string{"id", "email"}
			return sch(t)
		},
		seed:  `INSERT INTO people VALUES (1, 'a')`,
		check: `SELECT count(*) = 1 FROM people`,
	},
	{
		name: "add, change and drop checks",
		from: func() *schema.Schema {
			t := tbl("people", col("id", "int8", true), col("age", "int4", false))
			t.Checks = []*schema.Check{{Name: "ck_people_age", Expr: "age >= 0"}, {Name: "ck_people_gone", Expr: "id > 0"}}
			return sch(t)
		},
		to: func() *schema.Schema {
			t := tbl("people", col("id", "int8", true), col("age", "int4", false))
			t.Checks = []*schema.Check{{Name: "ck_people_age", Expr: "age >= 18"}, {Name: "ck_people_new", Expr: "age < 200"}}
			return sch(t)
		},
		seed:  `INSERT INTO people VALUES (1, 30)`,
		check: `SELECT count(*) = 1 FROM people WHERE age = 30`,
	},
	{
		name:  "replace the unique a foreign key references",
		from:  func() *schema.Schema { return sch(orgs("uq_orgs_code"), members(memberFK())) },
		to:    func() *schema.Schema { return sch(orgs("uq_orgs_code_v2"), members(memberFK())) },
		seed:  `INSERT INTO orgs VALUES (1, 'x'); INSERT INTO members VALUES (1, 'x')`,
		check: `SELECT count(*) = 1 FROM members JOIN orgs ON orgs.code = members.org_code`,
	},
	{
		name: "make a foreign key deferrable and cascading",
		from: func() *schema.Schema { return sch(orgs("uq_orgs_code"), members(memberFK())) },
		to: func() *schema.Schema {
			fk := memberFK()
			fk.Deferrable, fk.OnDelete = true, schema.Cascade
			return sch(orgs("uq_orgs_code"), members(fk))
		},
		seed:  `INSERT INTO orgs VALUES (1, 'x'); INSERT INTO members VALUES (1, 'x')`,
		check: `SELECT count(*) = 1 FROM members`,
	},
	{
		name:  "add a foreign key to a table created in the same plan",
		from:  func() *schema.Schema { return sch(members(nil)) },
		to:    func() *schema.Schema { return sch(orgs("uq_orgs_code"), members(memberFK())) },
		wants: `CREATE TABLE "orgs"`,
	},
	{
		name: "drop two tables, one referencing the other",
		from: func() *schema.Schema {
			a := tbl("a_parents", col("id", "int8", true))
			b := tbl("b_children", col("id", "int8", true), col("parent_id", "int8", false))
			b.ForeignKeys = []*schema.ForeignKey{{Name: "fk_b_children_parent_id", Columns: []string{"parent_id"},
				RefTable: "a_parents", RefColumns: []string{"id"}}}
			return sch(a, b)
		},
		to:    func() *schema.Schema { return sch(tbl("keep", col("id", "int8", true))) },
		wants: `DROP TABLE "a_parents"`,
	},
	{
		name: "rename a column, keeping its values and its index",
		from: func() *schema.Schema {
			t := tbl("people", col("id", "int8", true), col("nick", "text", false))
			t.Indexes = []*schema.Index{{Name: "ix_people_nick", Columns: key("nick")}}
			return sch(t)
		},
		to: func() *schema.Schema {
			c := col("handle", "text", false)
			c.RenamedFrom = "nick"
			t := tbl("people", col("id", "int8", true), c)
			t.Indexes = []*schema.Index{{Name: "ix_people_nick", Columns: key("handle")}}
			return sch(t)
		},
		seed:  `INSERT INTO people VALUES (1, 'ada')`,
		check: `SELECT handle = 'ada' FROM people WHERE id = 1`,
		wants: "RENAME COLUMN",
	},
	{
		name: "rename a column and retype it in one plan",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), vcol("nick", 10))) },
		to: func() *schema.Schema {
			c := vcol("handle", 40)
			c.RenamedFrom = "nick"
			return sch(tbl("people", col("id", "int8", true), c))
		},
		seed:  `INSERT INTO people VALUES (1, 'ada')`,
		check: `SELECT handle = 'ada' FROM people WHERE id = 1`,
		wants: "_storm_rebuild_people",
	},
	{
		name: "rename a referenced table, keeping its rows and the key into it",
		from: func() *schema.Schema { return sch(orgs("uq_orgs_code"), members(memberFK())) },
		to: func() *schema.Schema {
			o := orgs("uq_orgs_code")
			o.Name, o.RenamedFrom = "teams", "orgs"
			fk := memberFK()
			fk.RefTable = "teams"
			return sch(o, members(fk))
		},
		seed:  `INSERT INTO orgs VALUES (1, 'x'); INSERT INTO members VALUES (1, 'x')`,
		check: `SELECT count(*) = 1 FROM teams JOIN members ON members.org_code = teams.code`,
		wants: "RENAME TO",
	},
	{
		name: "change a generated column's expression, under an index",
		from: func() *schema.Schema {
			t := tbl("people", col("id", "int8", true), col("email", "text", true),
				&schema.Column{Name: "domain", Type: schema.Type{Name: "text"}, Generated: "substr(email, instr(email, '@') + 1)"})
			t.Indexes = []*schema.Index{{Name: "ix_people_domain", Columns: key("domain")}}
			return sch(t)
		},
		to: func() *schema.Schema {
			t := tbl("people", col("id", "int8", true), col("email", "text", true),
				&schema.Column{Name: "domain", Type: schema.Type{Name: "text"}, Generated: "lower(substr(email, instr(email, '@') + 1))"})
			t.Indexes = []*schema.Index{{Name: "ix_people_domain", Columns: key("domain")}}
			return sch(t)
		},
		seed:  `INSERT INTO people (id, email) VALUES (1, 'a@EXAMPLE.org')`,
		check: `SELECT domain = 'example.org' FROM people`,
	},
	{
		name: "stop generating a column, keeping its values",
		from: func() *schema.Schema {
			return sch(tbl("people", col("id", "int8", true), col("email", "text", true),
				&schema.Column{Name: "domain", Type: schema.Type{Name: "text"}, Generated: "substr(email, instr(email, '@') + 1)"}))
		},
		to: func() *schema.Schema {
			return sch(tbl("people", col("id", "int8", true), col("email", "text", true), col("domain", "text", false)))
		},
		seed:  `INSERT INTO people (id, email) VALUES (1, 'a@example.org')`,
		check: `SELECT domain = 'example.org' FROM people`,
	},
	{
		name: "make the key an identity past the rows it holds",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), col("n", "int8", false))) },
		to: func() *schema.Schema {
			id := col("id", "int8", true)
			id.Identity = true
			return sch(tbl("people", id, col("n", "int8", false)))
		},
		seed:  `INSERT INTO people VALUES (5, 1)`,
		check: `INSERT INTO people (n) VALUES (2) RETURNING id = 6`,
		wants: "AUTOINCREMENT",
	},
	{
		name: "stop the key being an identity",
		from: func() *schema.Schema {
			id := col("id", "int8", true)
			id.Identity = true
			return sch(tbl("people", id, col("n", "int8", false)))
		},
		to:    func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), col("n", "int8", false))) },
		seed:  `INSERT INTO people (n) VALUES (1)`,
		check: `SELECT count(*) = 1 FROM people WHERE id = 1`,
	},
	{
		name: "change an enum's labels",
		from: func() *schema.Schema {
			s := sch(tbl("people", col("id", "int8", true),
				&schema.Column{Name: "mood", Type: schema.Type{Name: "mood", Enum: true}, NotNull: true}))
			s.Enums = []*schema.Enum{{Name: "mood", Labels: []string{"sad", "happy"}}}
			return s
		},
		to: func() *schema.Schema {
			s := sch(tbl("people", col("id", "int8", true),
				&schema.Column{Name: "mood", Type: schema.Type{Name: "mood", Enum: true}, NotNull: true}))
			s.Enums = []*schema.Enum{{Name: "mood", Labels: []string{"meh", "sad", "ok", "happy"}}}
			return s
		},
		seed:  `INSERT INTO people VALUES (1, 'sad')`,
		check: `SELECT count(*) = 1 FROM people WHERE mood = 'sad'`,
		wants: "'meh'",
	},
	{
		// THE ONE THAT WOULD LOSE DATA if enforcement were on: rebuilding a
		// parent drops it, and DROP TABLE under foreign_keys = ON deletes its
		// rows first — cascading into every child declared ON DELETE CASCADE.
		name: "rebuild a parent without touching its cascading children",
		from: func() *schema.Schema {
			fk := memberFK()
			fk.OnDelete = schema.Cascade
			return sch(orgs("uq_orgs_code"), members(fk))
		},
		to: func() *schema.Schema {
			o := orgs("uq_orgs_code")
			o.Columns = append(o.Columns, col("label", "text", false))
			o.Checks = []*schema.Check{{Name: "ck_orgs_code", Expr: "code <> ''"}}
			fk := memberFK()
			fk.OnDelete = schema.Cascade
			return sch(o, members(fk))
		},
		seed:  `INSERT INTO orgs VALUES (1, 'x'); INSERT INTO members VALUES (1, 'x'), (2, 'x')`,
		check: `SELECT count(*) = 2 FROM members`,
		wants: "_storm_rebuild_orgs",
	},
}

func ctxBG() context.Context { return context.Background() }

// conn is one connection of a fresh database, which is what AutoSQLite and
// normalisation need: ATTACH and PRAGMA are per-connection.
func conn(t *testing.T) (*sql.DB, migrate.Conn) {
	t.Helper()
	db := open(t)
	c, err := db.Conn(ctxBG())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return db, sqlitedrv.New(c)
}

func scalar(t *testing.T, c migrate.Conn, q string) int64 {
	t.Helper()
	var got int64
	rows, err := c.Query(ctxBG(), q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	type values interface{ Values() []any }
	if rows.Next() {
		v := rows.(values).Values()
		if n, ok := v[0].(int64); ok {
			got = n
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return got
}

func TestSQLiteMigrationsConverge(t *testing.T) {
	for _, c := range migCases {
		t.Run(c.name, func(t *testing.T) {
			_, cn := conn(t)
			all := migrate.AutoOptions{AllowDestructive: true}
			if _, err := migrate.AutoSQLite(ctxBG(), cn, c.from(), all); err != nil {
				t.Fatalf("set up from: %v", err)
			}
			if c.seed != "" {
				if _, err := cn.Exec(ctxBG(), c.seed, nil); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			res, err := migrate.AutoSQLite(ctxBG(), cn, c.to(), all)
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if res.Empty() {
				t.Fatal("the migration did nothing")
			}
			if c.wants != "" && !strings.Contains(res.SQL(), c.wants) {
				t.Fatalf("the plan does not contain %q, so this case tested nothing:\n%s", c.wants, res.SQL())
			}
			left, err := migrate.ForSQLite(ctxBG(), cn, c.to(), migrate.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if !left.Empty() {
				t.Fatalf("after applying\n%s\nthe database still differs from the model:\n%s", res.SQL(), left.SQL())
			}
			if c.check != "" {
				if got := scalar(t, cn, c.check); got != 1 {
					t.Fatalf("check %q failed after\n%s", c.check, res.SQL())
				}
			}
			if n := scalar(t, cn, `SELECT count(*) FROM sqlite_schema WHERE name LIKE '\_storm\_rebuild\_%' ESCAPE '\'`); n != 0 {
				t.Fatalf("a rebuild left %d table(s) behind", n)
			}
		})
	}
}

// The plan back is a migration too: the reverse of the forward plan, written
// as a FILE and run as one, takes the database back to where it started.
func TestSQLiteMigrationsReverse(t *testing.T) {
	for _, c := range migCases {
		if strings.Contains(c.name, "generated") || strings.Contains(c.name, "two tables") {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			db, cn := conn(t)
			all := migrate.AutoOptions{AllowDestructive: true}
			if _, err := migrate.AutoSQLite(ctxBG(), cn, c.from(), all); err != nil {
				t.Fatalf("set up from: %v", err)
			}
			fwd, err := migrate.ForSQLite(ctxBG(), cn, c.to(), migrate.Options{})
			if err != nil {
				t.Fatal(err)
			}
			back, err := fwd.Reverse()
			if err != nil {
				t.Fatal(err)
			}
			_ = db
			for _, f := range []migrate.Plan{fwd, back} {
				if _, err := cn.Exec(ctxBG(), f.SQL(), nil); err != nil {
					t.Fatalf("%v\n%s", err, f.SQL())
				}
			}
			left, err := migrate.ForSQLite(ctxBG(), cn, c.from(), migrate.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if !left.Empty() {
				t.Fatalf("after\n%s\nand back\n%s\nthe database still differs from where it started:\n%s",
					fwd.SQL(), back.SQL(), left.SQL())
			}
		})
	}
}

// The guard: a plan that can lose data is refused WHOLE, naming each step, and
// nothing is applied — even though on SQLite the step is a table rebuild.
func TestSQLiteDestructivePlanIsRefused(t *testing.T) {
	_, cn := conn(t)
	from := func() *schema.Schema {
		t := tbl("people", col("id", "int8", true), col("email", "text", true), col("nick", "text", false))
		t.Indexes = []*schema.Index{{Name: "ix_people_nick", Columns: key("nick")}}
		return sch(t)
	}
	if _, err := migrate.AutoSQLite(ctxBG(), cn, from(), migrate.AutoOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cn.Exec(ctxBG(), `INSERT INTO people VALUES (1, 'a', 'ada')`, nil); err != nil {
		t.Fatal(err)
	}
	to := func() *schema.Schema {
		t := tbl("people", col("id", "int8", true), col("email", "text", true), col("age", "int4", false))
		return sch(t)
	}

	_, err := migrate.AutoSQLite(ctxBG(), cn, to(), migrate.AutoOptions{})
	var de *migrate.DestructiveError
	if !errors.As(err, &de) {
		t.Fatalf("dropping a column was not refused: %v", err)
	}
	kinds := map[string]bool{}
	for _, s := range de.Destructive() {
		kinds[s.Object] = true
		if s.Why == "" {
			t.Errorf("a refused step has no reason: %+v", s)
		}
	}
	if !kinds[migrate.ObjectColumn] || !kinds[migrate.ObjectIndex] {
		t.Errorf("the refusal does not name the column and the index separately: %v", de)
	}
	if n := scalar(t, cn, `SELECT count(*) FROM pragma_table_info('people') WHERE name = 'nick'`); n != 1 {
		t.Fatal("a refused plan was applied anyway")
	}

	// Accepting only the index is not enough: the dropped column is still refused.
	onlyIndex := func(c migrate.Change) bool { return c.Object == migrate.ObjectIndex }
	if _, err := migrate.AutoSQLite(ctxBG(), cn, to(), migrate.AutoOptions{Accept: onlyIndex}); !errors.As(err, &de) {
		t.Fatalf("Accept let a dropped column through: %v", err)
	}
	both := func(c migrate.Change) bool {
		return c.Object == migrate.ObjectIndex || c.Object == migrate.ObjectColumn
	}
	if _, err := migrate.AutoSQLite(ctxBG(), cn, to(), migrate.AutoOptions{Accept: both}); err != nil {
		t.Fatalf("accepting both kinds still refused: %v", err)
	}
	if n := scalar(t, cn, `SELECT count(*) FROM people WHERE email = 'a' AND age IS NULL`); n != 1 {
		t.Fatal("the row did not survive the rebuild")
	}
}

// A migration that would leave a key pointing at nothing is rolled back, not
// committed: enforcement is off while a rebuild runs, and foreign_key_check
// is what stands in for it.
func TestSQLiteMigrationLeavingADanglingKeyRollsBack(t *testing.T) {
	_, cn := conn(t)
	all := migrate.AutoOptions{AllowDestructive: true}
	if _, err := migrate.AutoSQLite(ctxBG(), cn, sch(orgs("uq_orgs_code"), members(nil)), all); err != nil {
		t.Fatal(err)
	}
	if _, err := cn.Exec(ctxBG(), `INSERT INTO members VALUES (1, 'nobody')`, nil); err != nil {
		t.Fatal(err)
	}
	_, err := migrate.AutoSQLite(ctxBG(), cn, sch(orgs("uq_orgs_code"), members(memberFK())), all)
	if err == nil || !strings.Contains(err.Error(), "points at nothing") {
		t.Fatalf("a dangling key was committed: %v", err)
	}
	if n := scalar(t, cn, `SELECT count(*) FROM pragma_foreign_key_list('members')`); n != 0 {
		t.Fatal("the key was added despite the rollback")
	}
	if n := scalar(t, cn, `PRAGMA foreign_keys`); n != 1 {
		t.Fatal("foreign key enforcement was left off")
	}
}
