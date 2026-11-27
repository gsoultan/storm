// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/v2/migrate"
	"github.com/gsoultan/storm/v2/schema"
)

// Convergence: for each kind of schema change, a database at `from` is
// migrated to `to`, and then the differ must find NOTHING left to do. That is
// the property every other test of the differ approximates: a step PostgreSQL
// refuses fails the migration, a step that is missing shows up in the second
// plan, and a step that does the wrong thing shows up there too.
//
// The schemas are built by functions because a diff normalises what it is
// given in place.

type convergeCase struct {
	name     string
	from, to func() *schema.Schema
	// seed runs against `from` before the migration, and check against `to`
	// after it: for the changes whose point is the data they keep.
	seed, check string
}

func pk(t *schema.Table, cols ...string) *schema.Table { t.PrimaryKey = cols; return t }

func idx(t *schema.Table, ix ...*schema.Index) *schema.Table { t.Indexes = ix; return t }

func key(cols ...string) []schema.IndexColumn {
	out := make([]schema.IndexColumn, len(cols))
	for i, c := range cols {
		out[i] = schema.IndexColumn{Name: c}
	}
	return out
}

func vcol(name string, size int) *schema.Column {
	return &schema.Column{Name: name, Type: schema.Type{Name: "varchar", Size: size}}
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

var convergeCases = []convergeCase{
	{
		name: "drop a column with an index, a unique and a check on it",
		from: func() *schema.Schema {
			t := tbl("people", col("id", "int8", true), col("email", "text", true), vcol("nick", 40))
			t.Indexes = []*schema.Index{{Name: "ix_people_nick", Columns: key("nick")}}
			t.Uniques = []*schema.Unique{{Name: "uq_people_email_nick", Columns: []string{"email", "nick"}}}
			t.Checks = []*schema.Check{{Name: "ck_people_nick", Expr: "nick <> ''"}}
			return sch(t)
		},
		to: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), col("email", "text", true))) },
	},
	{
		name: "add a column with an index and a comment",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true))) },
		to: func() *schema.Schema {
			c := vcol("nick", 40)
			c.Comment = "what they go by"
			return sch(idx(tbl("people", col("id", "int8", true), c),
				&schema.Index{Name: "ix_people_nick", Columns: key("nick")}))
		},
	},
	{
		name: "retype, tighten and default a column",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), vcol("nick", 40))) },
		to: func() *schema.Schema {
			c := vcol("nick", 80)
			c.NotNull, c.Default, c.FillNulls = true, "'anon'::character varying", "'anon'"
			return sch(tbl("people", col("id", "int8", true), c))
		},
		seed:  `INSERT INTO people (id) VALUES (1)`,
		check: `SELECT count(*) = 1 FROM people WHERE nick = 'anon'`,
	},
	{
		name: "change, drop and add indexes",
		from: func() *schema.Schema {
			return sch(idx(tbl("people", col("id", "int8", true), col("a", "text", false), col("b", "text", false)),
				&schema.Index{Name: "ix_people_a", Columns: key("a")},
				&schema.Index{Name: "ix_people_gone", Columns: key("b")},
				&schema.Index{Name: "ix_people_u", Columns: key("a"), Unique: true}))
		},
		to: func() *schema.Schema {
			return sch(idx(tbl("people", col("id", "int8", true), col("a", "text", false), col("b", "text", false)),
				&schema.Index{Name: "ix_people_a", Columns: key("a", "b")},
				&schema.Index{Name: "ix_people_new", Columns: key("b"), Where: "b IS NOT NULL"},
				&schema.Index{Name: "ix_people_u", Columns: key("a")}))
		},
	},
	{
		name: "change the primary key",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), col("email", "text", true))) },
		to: func() *schema.Schema {
			return sch(pk(tbl("people", col("id", "int8", true), col("email", "text", true)), "id", "email"))
		},
	},
	{
		name: "drop the primary key",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true))) },
		to:   func() *schema.Schema { return sch(pk(tbl("people", col("id", "int8", true)))) },
	},
	{
		name: "add a primary key",
		from: func() *schema.Schema { return sch(pk(tbl("people", col("id", "int8", true)))) },
		to:   func() *schema.Schema { return sch(tbl("people", col("id", "int8", true))) },
	},
	{
		name: "replace the unique a foreign key references",
		from: func() *schema.Schema { return sch(orgs("uq_orgs_code"), members(memberFK())) },
		to:   func() *schema.Schema { return sch(orgs("uq_orgs_code_v2"), members(memberFK())) },
	},
	{
		name: "make a foreign key deferrable and cascading",
		from: func() *schema.Schema { return sch(orgs("uq_orgs_code"), members(memberFK())) },
		to: func() *schema.Schema {
			fk := memberFK()
			fk.Deferrable, fk.OnDelete = true, schema.Cascade
			return sch(orgs("uq_orgs_code"), members(fk))
		},
	},
	{
		name: "add a foreign key to a table created in the same plan",
		from: func() *schema.Schema { return sch(members(nil)) },
		to:   func() *schema.Schema { return sch(orgs("uq_orgs_code"), members(memberFK())) },
	},
	{
		name: "drop two tables, one referencing the other",
		from: func() *schema.Schema {
			// b references a, and a sorts first: dropped in name order,
			// a's drop fails on b's key.
			a := tbl("a_parents", col("id", "int8", true))
			b := tbl("b_children", col("id", "int8", true), col("parent_id", "int8", false))
			b.ForeignKeys = []*schema.ForeignKey{{Name: "fk_b_children_parent_id", Columns: []string{"parent_id"},
				RefTable: "a_parents", RefColumns: []string{"id"}}}
			return sch(a, b)
		},
		to: func() *schema.Schema { return sch() },
	},
	{
		name: "rename a column, keeping its values and its index",
		from: func() *schema.Schema {
			return sch(idx(tbl("people", col("id", "int8", true), col("nick", "text", false)),
				&schema.Index{Name: "ix_people_nick", Columns: key("nick")}))
		},
		to: func() *schema.Schema {
			c := col("handle", "text", false)
			c.RenamedFrom = "nick"
			return sch(idx(tbl("people", col("id", "int8", true), c),
				&schema.Index{Name: "ix_people_nick", Columns: key("handle")}))
		},
		seed:  `INSERT INTO people VALUES (1, 'ada')`,
		check: `SELECT handle = 'ada' FROM people WHERE id = 1`,
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
	},
	{
		name: "change table and column comments",
		from: func() *schema.Schema {
			t := tbl("people", col("id", "int8", true), col("nick", "text", false))
			t.Comment, t.Columns[1].Comment = "old", "old"
			return sch(t)
		},
		to: func() *schema.Schema {
			t := tbl("people", col("id", "int8", true), col("nick", "text", false))
			t.Comment = "new"
			return sch(t)
		},
	},
	{
		name: "change a generated column's expression, under an index",
		from: func() *schema.Schema {
			return sch(idx(tbl("people", col("id", "int8", true), col("email", "text", true),
				&schema.Column{Name: "domain", Type: schema.Type{Name: "text"}, Generated: "split_part(email, '@', 2)"}),
				&schema.Index{Name: "ix_people_domain", Columns: key("domain")}))
		},
		to: func() *schema.Schema {
			return sch(idx(tbl("people", col("id", "int8", true), col("email", "text", true),
				&schema.Column{Name: "domain", Type: schema.Type{Name: "text"}, Generated: "lower(split_part(email, '@', 2))"}),
				&schema.Index{Name: "ix_people_domain", Columns: key("domain")}))
		},
		seed:  `INSERT INTO people (id, email) VALUES (1, 'a@EXAMPLE.org')`,
		check: `SELECT domain = 'example.org' FROM people`,
	},
	{
		name: "stop generating a column, keeping its values",
		from: func() *schema.Schema {
			return sch(tbl("people", col("id", "int8", true), col("email", "text", true),
				&schema.Column{Name: "domain", Type: schema.Type{Name: "text"}, Generated: "split_part(email, '@', 2)"}))
		},
		to: func() *schema.Schema {
			return sch(tbl("people", col("id", "int8", true), col("email", "text", true), col("domain", "text", false)))
		},
		seed:  `INSERT INTO people (id, email) VALUES (1, 'a@example.org')`,
		check: `SELECT domain = 'example.org' FROM people`,
	},
	{
		name: "make a column an identity past the rows it holds",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), col("n", "int8", true))) },
		to: func() *schema.Schema {
			n := col("n", "int8", true)
			n.Identity = true
			return sch(tbl("people", col("id", "int8", true), n))
		},
		seed:  `INSERT INTO people VALUES (1, 1), (2, 2)`,
		check: `INSERT INTO people (id) VALUES (3) RETURNING n = 3`,
	},
	{
		name: "stop a column being an identity",
		from: func() *schema.Schema {
			n := col("n", "int8", true)
			n.Identity = true
			return sch(tbl("people", col("id", "int8", true), n))
		},
		to: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), col("n", "int8", true))) },
	},
	{
		name: "make a column a serial past the rows it holds",
		from: func() *schema.Schema { return sch(tbl("people", col("id", "int8", true), col("n", "int4", true))) },
		to: func() *schema.Schema {
			n := col("n", "int4", true)
			n.Serial = true
			return sch(tbl("people", col("id", "int8", true), n))
		},
		seed:  `INSERT INTO people VALUES (1, 5)`,
		check: `INSERT INTO people (id) VALUES (2) RETURNING n = 6`,
	},
	{
		name: "turn a serial into an identity",
		from: func() *schema.Schema {
			n := col("n", "int8", true)
			n.Serial = true
			return sch(tbl("people", col("id", "int8", true), n))
		},
		to: func() *schema.Schema {
			n := col("n", "int8", true)
			n.Identity = true
			return sch(tbl("people", col("id", "int8", true), n))
		},
	},
	{
		name: "add, replace and drop exclusion constraints",
		from: func() *schema.Schema {
			t := tbl("bookings", col("id", "int8", true), col("room", "int4", true), col("during", "tstzrange", true))
			t.Excludes = []*schema.Exclude{
				{Method: "gist", Name: "ex_bookings_a", Parts: []schema.ExcludePart{{Column: "room", Operator: "="}, {Column: "during", Operator: "&&"}}},
				{Method: "gist", Name: "ex_bookings_gone", Parts: []schema.ExcludePart{{Column: "during", Operator: "&&"}}},
			}
			return sch(t)
		},
		to: func() *schema.Schema {
			t := tbl("bookings", col("id", "int8", true), col("room", "int4", true), col("during", "tstzrange", true))
			t.Excludes = []*schema.Exclude{
				{Method: "gist", Name: "ex_bookings_a", Parts: []schema.ExcludePart{{Column: "room", Operator: "="}, {Column: "during", Operator: "-|-"}}},
				{Method: "gist", Name: "ex_bookings_new", Parts: []schema.ExcludePart{{Column: "id", Operator: "="}, {Column: "during", Operator: "&&"}}},
			}
			return sch(t)
		},
	},
	{
		name: "insert an enum label in the middle",
		from: func() *schema.Schema {
			s := sch()
			s.Enums = []*schema.Enum{{Name: "mood", Labels: []string{"sad", "happy"}}}
			return s
		},
		to: func() *schema.Schema {
			s := sch()
			s.Enums = []*schema.Enum{{Name: "mood", Labels: []string{"meh", "sad", "ok", "happy"}}}
			return s
		},
	},
	{
		name: "drop and retype columns under views",
		from: func() *schema.Schema {
			s := sch(tbl("people", col("id", "int8", true), vcol("nick", 40), col("gone", "text", false)))
			s.Views = []*schema.View{
				{Name: "nicks", Query: "SELECT id, nick, gone FROM people"},
				{Name: "nick_count", Query: "SELECT count(*) AS n FROM nicks"},
			}
			return s
		},
		to: func() *schema.Schema {
			s := sch(tbl("people", col("id", "int8", true), vcol("nick", 80)))
			s.Views = []*schema.View{
				{Name: "nicks", Query: "SELECT id, nick FROM people"},
				{Name: "nick_count", Query: "SELECT count(*) AS n FROM nicks"},
			}
			return s
		},
	},
}

func TestConvergence(t *testing.T) {
	for _, online := range []bool{false, true} {
		for i, c := range convergeCases {
			name := c.name
			if online {
				name += " online"
			}
			t.Run(name, func(t *testing.T) { converge(t, i, c, online) })
		}
	}
}

// converge is one case. Online runs it with Concurrently, which moves index
// builds and drops out of the plan's transaction: the same changes, applied in
// a different order, have to arrive at the same place.
func converge(t *testing.T, i int, c convergeCase, online bool) {
	ns := "storm_conv_" + string(rune('a'+i/26)) + string(rune('a'+i%26))
	if online {
		ns += "_online"
	}
	ctx, conn := conn(t, ns)
	all := migrate.AutoOptions{Schema: ns, AllowDestructive: true}

	if _, err := migrate.Auto(ctx, conn, c.from(), all); err != nil {
		t.Fatalf("set up from: %v", err)
	}
	if c.seed != "" {
		exec(t, ctx, conn, ns, c.seed)
	}
	res, err := migrate.Auto(ctx, conn, c.to(), migrate.AutoOptions{Schema: ns, AllowDestructive: true, Concurrently: online})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Logf("plan:\n%s", res.SQL())
	if res.Empty() {
		t.Fatal("the migration did nothing")
	}
	left, err := migrate.ForWith(ctx, conn, ns, c.to(), migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !left.Empty() {
		t.Fatalf("after applying\n%s\nthe database still differs from the model:\n%s", res.SQL(), left.SQL())
	}
	if c.check != "" {
		var ok bool
		if err := inSchema(ctx, conn, ns, func() error { return conn.QueryRow(ctx, c.check).Scan(&ok) }); err != nil {
			t.Fatalf("check: %v", err)
		}
		if !ok {
			t.Fatalf("check %q failed after\n%s", c.check, res.SQL())
		}
	}
}

// The plan back is a migration too: from `to`, the reverse of the forward
// plan has to take the database back to where it started.
func TestConvergenceBack(t *testing.T) {
	for i, c := range convergeCases {
		if strings.Contains(c.name, "enum") || strings.Contains(c.name, "generated") ||
			strings.Contains(c.name, "identity") || strings.Contains(c.name, "serial") {
			continue // no way back without a human, by design: see the plan's marks
		}
		t.Run(c.name, func(t *testing.T) {
			ns := "storm_back_" + string(rune('a'+i/26)) + string(rune('a'+i%26))
			ctx, conn := conn(t, ns)
			all := migrate.AutoOptions{Schema: ns, AllowDestructive: true}
			if _, err := migrate.Auto(ctx, conn, c.from(), all); err != nil {
				t.Fatalf("set up from: %v", err)
			}
			fwd, err := migrate.ForWith(ctx, conn, ns, c.to(), migrate.Options{})
			if err != nil {
				t.Fatal(err)
			}
			back, err := fwd.Reverse()
			if err != nil {
				t.Fatal(err)
			}
			exec(t, ctx, conn, ns, fwd.SQL())
			exec(t, ctx, conn, ns, back.SQL())
			left, err := migrate.ForWith(ctx, conn, ns, c.from(), migrate.Options{})
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

func inSchema(ctx context.Context, c *caduceus.Conn, ns string, f func() error) error {
	if _, err := c.Exec(ctx, "SET search_path TO "+ns); err != nil {
		return err
	}
	defer c.Exec(ctx, "RESET search_path")
	return f()
}

func exec(t *testing.T, ctx context.Context, c *caduceus.Conn, ns, sql string) {
	t.Helper()
	if err := inSchema(ctx, c, ns, func() error { _, err := c.Exec(ctx, sql); return err }); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}
