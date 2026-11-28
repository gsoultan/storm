// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlitecheck

import (
	"testing"
	"time"

	"github.com/gsoultan/storm/v2"
	"github.com/gsoultan/storm/v2/migrate"
	"github.com/gsoultan/storm/v2/schema"
	"github.com/gsoultan/storm/v2/schema/sqlite"
)

// Introspection, against what a real model builds to: every construct
// compile/liteddl renders — the rowid, an enum's check, a varchar's length
// check, a document's json_valid, a numeric, a uuid key, a self-reference, a
// partial unique, an expression unique, a descending index, a check — is
// applied, read back, and must diff as nothing at all.

type rtStatus string

func (rtStatus) EnumValues() []string { return []string{"pending", "active", "gone"} }

type rtOrg struct {
	storm.Model
	Name   string
	Parent *rtOrg
}

func (o *rtOrg) Schema(t *storm.Table) {
	t.Col(&o.Name).Size(200)
	t.Col(&o.Parent).OnDelete(storm.Cascade)
	t.Unique(&o.Name)
}

type rtUser struct {
	storm.Model
	Email     string
	Name      string
	Status    rtStatus
	Prefs     storm.JSON
	Balance   storm.Decimal
	Active    bool
	Age       *int16
	Org       rtOrg
	DeletedAt *time.Time
}

func (u *rtUser) Schema(t *storm.Table) {
	t.SoftDelete(&u.DeletedAt)
	t.Col(&u.Email).Size(320)
	t.Col(&u.Name).Size(120)
	t.Col(&u.Status).Default("'pending'")
	t.Col(&u.Prefs).Default("'{}'")
	t.Col(&u.Balance).Numeric(15, 4).Default("0")
	t.Col(&u.Active).Default("true")
	t.Col(&u.Org).OnDelete(storm.Restrict)
	t.Unique(&u.Email)
	t.Unique(storm.Lower(&u.Name))
	t.Index(&u.Org, storm.Desc(&u.CreatedAt))
	t.Index(&u.Status).Where("status <> 'gone'")
	t.Check("age IS NULL OR age BETWEEN 0 AND 150")
}

type rtCounter struct {
	ID    int64
	Label string
}

func (c *rtCounter) Schema(t *storm.Table) {
	t.PrimaryKey(&c.ID)
	t.Col(&c.ID).Identity()
}

func rtModel(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := storm.Build(&rtOrg{}, &rtUser{}, &rtCounter{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAModelRoundTripsThroughIntrospection(t *testing.T) {
	_, cn := conn(t)
	res, err := migrate.AutoSQLite(ctxBG(), cn, rtModel(t), migrate.AutoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Empty() {
		t.Fatal("an empty database needed no migration")
	}
	again, err := migrate.ForSQLite(ctxBG(), cn, rtModel(t), migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("re-diff after applying is not empty — normalisation and introspection "+
			"disagree:\n%s\napplied:\n%s", again.SQL(), res.SQL())
	}

	// And what came back reads as the model, not as SQLite's storage of it.
	got, err := schemasqlite.Introspect(ctxBG(), cn, "")
	if err != nil {
		t.Fatal(err)
	}
	users := got.Table("rt_users")
	if users == nil {
		t.Fatalf("no rt_users among %d tables", len(got.Tables))
	}
	if c := users.Column("email"); c == nil || c.Type.Name != schema.TypeVarchar || c.Type.Size != 320 {
		t.Errorf("email came back as %+v, want varchar(320) — the length check is the type", c)
	}
	if c := users.Column("prefs"); c == nil || c.Type.Name != schema.TypeJSONB {
		t.Errorf("prefs came back as %+v, want jsonb — json_valid is the type", c)
	}
	if c := users.Column("balance"); c == nil || c.Type.Precision != 15 || c.Type.Scale != 4 {
		t.Errorf("balance came back as %+v, want numeric(15,4)", c)
	}
	if c := users.Column("created_at"); c == nil || c.Default != "now()" {
		t.Errorf("created_at's default came back as %q, want now()", c.Default)
	}
	if len(users.ForeignKeys) != 1 || users.ForeignKeys[0].RefTable != "rt_orgs" {
		t.Errorf("foreign keys: %+v", users.ForeignKeys)
	}
	counter := got.Table("rt_counters")
	if c := counter.Column("id"); c == nil || !c.Identity || c.Type.Name != schema.TypeInt8 {
		t.Errorf("the rowid came back as %+v, want an int8 identity", c)
	}
	var partial, expr bool
	for _, ix := range users.Indexes {
		partial = partial || ix.Where != ""
		for _, k := range ix.Columns {
			expr = expr || k.Expr
		}
	}
	if !partial || !expr {
		t.Errorf("indexes lost their predicate or expression: %+v", users.Indexes)
	}
}
