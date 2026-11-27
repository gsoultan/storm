// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package migrate

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/schema"
)

// Every kind of schema change a model can make, rendered for each SQL back
// end the differ supports. The live half — that PostgreSQL accepts each plan
// and that the database then agrees with the model — is converge_test.go.

var dialects = []Dialect{Postgres, MSSQL, Oracle}

func planFor(t *testing.T, d Dialect, from, to *schema.Schema) Plan {
	t.Helper()
	p, err := DiffFor(from, to, d)
	if err != nil {
		t.Fatalf("%s: %v", d, err)
	}
	return p
}

// sqls is the plan's statements, one per change.
func sqls(p Plan) []string {
	out := make([]string, len(p.Changes))
	for i, c := range p.Changes {
		out[i] = c.SQL
	}
	return out
}

// at is the index of the first change containing all of parts, or -1.
func at(p Plan, parts ...string) int {
	for i, c := range p.Changes {
		all := true
		for _, s := range parts {
			if !strings.Contains(c.SQL, s) {
				all = false
				break
			}
		}
		if all {
			return i
		}
	}
	return -1
}

func mustAt(t *testing.T, p Plan, parts ...string) int {
	t.Helper()
	i := at(p, parts...)
	if i < 0 {
		t.Fatalf("no step contains %q; plan:\n%s", parts, p.SQL())
	}
	return i
}

func before(t *testing.T, p Plan, a, b []string) {
	t.Helper()
	if i, j := mustAt(t, p, a...), mustAt(t, p, b...); i >= j {
		t.Fatalf("%q must come before %q; plan:\n%s", a, b, p.SQL())
	}
}

func people(extra ...*schema.Column) *schema.Table {
	return mtbl("people", append([]*schema.Column{mcol("id", "int8", true), mcol("email", "text", true)}, extra...)...)
}

// The bug this file started from: dropping a column used to come before
// dropping its index and constraints, and every server drops those WITH the
// column, so the DROP INDEX that followed failed on an index already gone.
func TestDropColumnDropsItsIndexAndConstraintsFirst(t *testing.T) {
	for _, d := range dialects {
		from := people(mcol("nick", "varchar", false))
		from.Columns[2].Type.Size = 40
		from.Indexes = []*schema.Index{{Name: "ix_people_nick", Columns: []schema.IndexColumn{{Name: "nick"}}}}
		from.Uniques = []*schema.Unique{{Name: "uq_people_email_nick", Columns: []string{"email", "nick"}}}
		from.Checks = []*schema.Check{{Name: "ck_people_nick", Expr: "nick <> ''"}}
		p := planFor(t, d, msch(from), msch(people()))
		for _, obj := range []string{"ix_people_nick", "uq_people_email_nick", "ck_people_nick"} {
			before(t, p, []string{obj}, []string{"DROP COLUMN"})
		}
		if !p.Destructive() {
			t.Errorf("%s: dropping a column is not marked destructive", d)
		}
	}
}

func TestAddColumnComesBeforeTheIndexOnIt(t *testing.T) {
	for _, d := range dialects {
		to := people(mcol("nick", "text", false))
		to.Indexes = []*schema.Index{{Name: "ix_people_nick", Columns: []schema.IndexColumn{{Name: "nick"}}}}
		p := planFor(t, d, msch(people()), msch(to))
		before(t, p, []string{"ADD", "nick"}, []string{"INDEX", "ix_people_nick"})
	}
}

func TestIndexChangeIsDropAndCreate(t *testing.T) {
	for _, d := range dialects {
		mk := func(cols ...string) *schema.Schema {
			tb := people(mcol("nick", "text", false))
			ix := &schema.Index{Name: "ix_people_lookup"}
			for _, c := range cols {
				ix.Columns = append(ix.Columns, schema.IndexColumn{Name: c})
			}
			tb.Indexes = []*schema.Index{ix}
			return msch(tb)
		}
		p := planFor(t, d, mk("email"), mk("email", "nick"))
		before(t, p, []string{"DROP INDEX", "ix_people_lookup"}, []string{"CREATE INDEX", "ix_people_lookup"})
		if p.Destructive() {
			t.Errorf("%s: replacing an index is not data loss:\n%s", d, p.SQL())
		}
	}
}

func TestPrimaryKeyChanges(t *testing.T) {
	for _, d := range dialects {
		withPK := func(pk ...string) *schema.Schema {
			tb := people()
			tb.PrimaryKey = pk
			return msch(tb)
		}

		p := planFor(t, d, withPK("id"), withPK("id", "email"))
		before(t, p, []string{"people"}, []string{"ADD PRIMARY KEY"})
		if i := at(p, "ADD PRIMARY KEY", "email"); i < 0 {
			t.Fatalf("%s: new key not added:\n%s", d, p.SQL())
		}
		if p.Destructive() {
			t.Errorf("%s: replacing a key is not data loss:\n%s", d, p.SQL())
		}

		p = planFor(t, d, withPK("id"), withPK())
		if len(p.Changes) != 1 || !p.Changes[0].Destructive || p.Changes[0].Object != ObjectPrimaryKey {
			t.Errorf("%s: dropping the key should be one destructive primary-key step:\n%s", d, p.SQL())
		}

		p = planFor(t, d, withPK(), withPK("id"))
		if len(p.Changes) != 1 || !strings.Contains(p.Changes[0].SQL, "ADD PRIMARY KEY") {
			t.Errorf("%s: adding a key should be one step:\n%s", d, p.SQL())
		}

		if p := planFor(t, d, withPK("id"), withPK("id")); !p.Empty() {
			t.Errorf("%s: an unchanged key produced steps:\n%s", d, p.SQL())
		}
	}
}

// The servers name an inline key themselves, so the drop has to find it.
func TestDropPrimaryKeyFindsTheServersName(t *testing.T) {
	from, to := msch(people()), msch(people())
	to.Tables[0].PrimaryKey = nil
	if s := planFor(t, Postgres, from, to).Changes[0].SQL; !strings.Contains(s, "pg_constraint") || !strings.Contains(s, "contype = 'p'") {
		t.Errorf("postgres drop does not read the catalogue:\n%s", s)
	}
	from, to = msch(people()), msch(people())
	to.Tables[0].PrimaryKey = nil
	if s := planFor(t, MSSQL, from, to).Changes[0].SQL; !strings.Contains(s, "sys.key_constraints") {
		t.Errorf("mssql drop does not read the catalogue:\n%s", s)
	}
	from, to = msch(people()), msch(people())
	to.Tables[0].PrimaryKey = nil
	if s := planFor(t, Oracle, from, to).Changes[0].SQL; s != `ALTER TABLE "people" DROP PRIMARY KEY` {
		t.Errorf("oracle drop = %q", s)
	}
}

func orgsAndPeople(uniqueName string) *schema.Schema {
	orgs := mtbl("orgs", mcol("id", "int8", true), mcol("code", "text", true))
	orgs.Uniques = []*schema.Unique{{Name: uniqueName, Columns: []string{"code"}}}
	ppl := people(mcol("org_code", "text", false))
	ppl.ForeignKeys = []*schema.ForeignKey{{Name: "fk_people_org_code", Columns: []string{"org_code"},
		RefTable: "orgs", RefColumns: []string{"code"}}}
	return msch(orgs, ppl)
}

// A foreign key depends on the key it references. Replacing that key while
// the foreign key stands is refused by every server, so the foreign key goes
// first and comes back last, though it did not itself change.
func TestReplacedKeyTakesItsForeignKeysWithIt(t *testing.T) {
	for _, d := range dialects {
		p := planFor(t, d, orgsAndPeople("uq_orgs_code"), orgsAndPeople("uq_orgs_code_v2"))
		before(t, p, []string{"DROP CONSTRAINT", "fk_people_org_code"}, []string{"DROP CONSTRAINT", "uq_orgs_code"})
		before(t, p, []string{"ADD CONSTRAINT", "uq_orgs_code_v2"}, []string{"ADD CONSTRAINT", "fk_people_org_code"})
	}
}

func TestForeignKeyFactsAreCompared(t *testing.T) {
	for _, d := range dialects {
		for name, change := range map[string]func(*schema.ForeignKey){
			"on delete":  func(f *schema.ForeignKey) { f.OnDelete = schema.Cascade },
			"on update":  func(f *schema.ForeignKey) { f.OnUpdate = schema.Cascade },
			"deferrable": func(f *schema.ForeignKey) { f.Deferrable = true },
			"target":     func(f *schema.ForeignKey) { f.RefColumns = []string{"id"}; f.Columns = []string{"email"} },
		} {
			to := orgsAndPeople("uq_orgs_code")
			change(to.Table("people").ForeignKeys[0])
			p := planFor(t, d, orgsAndPeople("uq_orgs_code"), to)
			t.Logf("%s %s:\n%s", d, name, p.SQL())
			before(t, p, []string{"DROP CONSTRAINT", "fk_people_org_code"}, []string{"ADD CONSTRAINT", "fk_people_org_code"})
		}
		// Dropped from the model: one destructive drop, no re-add.
		to := orgsAndPeople("uq_orgs_code")
		to.Table("people").ForeignKeys = nil
		p := planFor(t, d, orgsAndPeople("uq_orgs_code"), to)
		if len(p.Changes) != 1 || !p.Changes[0].Destructive || p.Changes[0].Object != ObjectForeignKey {
			t.Errorf("%s: a removed foreign key should be one destructive step:\n%s", d, p.SQL())
		}
	}
}

// An existing table gaining a key to a table this plan creates: the key has to
// wait for the table, wherever the two sort.
func TestNewForeignKeyWaitsForTheTableItReferences(t *testing.T) {
	for _, d := range dialects {
		from := msch(mtbl("a_items", mcol("id", "int8", true)))
		items := mtbl("a_items", mcol("id", "int8", true), mcol("owner_id", "int8", false))
		items.ForeignKeys = []*schema.ForeignKey{{Name: "fk_a_items_owner_id", Columns: []string{"owner_id"},
			RefTable: "z_owners", RefColumns: []string{"id"}}}
		to := msch(items, mtbl("z_owners", mcol("id", "int8", true)))
		p := planFor(t, d, from, to)
		before(t, p, []string{"CREATE TABLE", "z_owners"}, []string{"fk_a_items_owner_id"})
	}
}

// Two dropped tables where one references the other: dropping the referenced
// one first fails on the reference.
func TestDroppedTablesReferencingEachOther(t *testing.T) {
	for _, d := range dialects {
		a := mtbl("a_parents", mcol("id", "int8", true))
		b := mtbl("b_children", mcol("id", "int8", true), mcol("parent_id", "int8", false))
		b.ForeignKeys = []*schema.ForeignKey{{Name: "fk_b_children_parent_id", Columns: []string{"parent_id"},
			RefTable: "a_parents", RefColumns: []string{"id"}}}
		p := planFor(t, d, msch(a, b), msch())
		before(t, p, []string{"fk_b_children_parent_id"}, []string{"DROP TABLE", "a_parents"})
	}
}

func TestRenameColumnKeepsTheColumn(t *testing.T) {
	want := map[Dialect]string{
		Postgres: `ALTER TABLE "people" RENAME COLUMN "nick" TO "handle";`,
		MSSQL:    `EXEC sp_rename N'[people].[nick]', N'handle', N'COLUMN';`,
		Oracle:   `ALTER TABLE "people" RENAME COLUMN "nick" TO "handle"`,
	}
	for _, d := range dialects {
		from := people(mcol("nick", "text", false))
		from.Indexes = []*schema.Index{{Name: "ix_people_nick", Columns: []schema.IndexColumn{{Name: "nick"}}}}
		to := people(&schema.Column{Name: "handle", Type: schema.Type{Name: "text"}, RenamedFrom: "nick"})
		to.Indexes = []*schema.Index{{Name: "ix_people_nick", Columns: []schema.IndexColumn{{Name: "handle"}}}}
		p := planFor(t, d, msch(from), msch(to))
		if got := sqls(p); len(got) != 1 || got[0] != want[d] {
			t.Errorf("%s: a rename should be one step, and the index follows it:\n%s", d, p.SQL())
		}
		if p.Destructive() {
			t.Errorf("%s: a rename keeps the data", d)
		}

		// Once the database has the new name, RenamedFrom renders nothing.
		again := people(&schema.Column{Name: "handle", Type: schema.Type{Name: "text"}, RenamedFrom: "nick"})
		done := people(mcol("handle", "text", false))
		if p := planFor(t, d, msch(done), msch(again)); !p.Empty() {
			t.Errorf("%s: a rename already applied produced steps:\n%s", d, p.SQL())
		}
	}
}

func TestRenameTableKeepsTheTableAndItsReferences(t *testing.T) {
	want := map[Dialect]string{
		Postgres: `ALTER TABLE "orgs" RENAME TO "teams";`,
		MSSQL:    `EXEC sp_rename N'[orgs]', N'teams';`,
		Oracle:   `ALTER TABLE "orgs" RENAME TO "teams"`,
	}
	for _, d := range dialects {
		from := orgsAndPeople("uq_orgs_code")
		to := orgsAndPeople("uq_orgs_code")
		to.Table("orgs").Name, to.Table("orgs").RenamedFrom = "teams", "orgs"
		to.Table("people").ForeignKeys[0].RefTable = "teams"
		p := planFor(t, d, from, to)
		if got := sqls(p); len(got) != 1 || got[0] != want[d] {
			t.Errorf("%s: a table rename should be one step, the foreign key following it:\n%s", d, p.SQL())
		}

		back, err := p.Reverse()
		if err != nil {
			t.Fatal(err)
		}
		if got := sqls(back); len(got) != 1 || !strings.Contains(got[0], "teams") || !strings.Contains(got[0], "orgs") {
			t.Errorf("%s: the reverse of a rename should rename back:\n%s", d, back.SQL())
		}
	}
}

func TestRenamedColumnReversesToItsOldName(t *testing.T) {
	from := people(mcol("nick", "text", false))
	to := people(&schema.Column{Name: "handle", Type: schema.Type{Name: "text"}, RenamedFrom: "nick"})
	p := planFor(t, Postgres, msch(from), msch(to))
	back, err := p.Reverse()
	if err != nil {
		t.Fatal(err)
	}
	if got := sqls(back); len(got) != 1 || got[0] != `ALTER TABLE "people" RENAME COLUMN "handle" TO "nick";` {
		t.Errorf("reverse:\n%s", back.SQL())
	}
}

// A rename and a change to the renamed column are two steps, the rename first.
func TestRenameThenRetype(t *testing.T) {
	for _, d := range dialects {
		from := people(mcol("nick", "text", false))
		to := people(&schema.Column{Name: "handle", Type: schema.Type{Name: "text"}, NotNull: true, RenamedFrom: "nick",
			Default: "'x'"})
		p := planFor(t, d, msch(from), msch(to))
		before(t, p, []string{"nick", "handle"}, []string{"NOT NULL"})
	}
}

func TestCommentsAreCompared(t *testing.T) {
	withComments := func(table, column string) *schema.Schema {
		tb := people(&schema.Column{Name: "nick", Type: schema.Type{Name: "text"}, Comment: column})
		tb.Comment = table
		return msch(tb)
	}
	p := planFor(t, Postgres, withComments("", ""), withComments("people we know", "what they go by"))
	if got := sqls(p); len(got) != 2 ||
		got[0] != `COMMENT ON COLUMN "people"."nick" IS 'what they go by';` ||
		got[1] != `COMMENT ON TABLE "people" IS 'people we know';` {
		t.Errorf("comments set:\n%s", p.SQL())
	}
	p = planFor(t, Postgres, withComments("a", "b"), withComments("", ""))
	if got := sqls(p); len(got) != 2 || !strings.HasSuffix(got[0], "IS NULL;") || !strings.HasSuffix(got[1], "IS NULL;") {
		t.Errorf("comments cleared:\n%s", p.SQL())
	}
	// A new column arrives with its comment.
	p = planFor(t, Postgres, msch(people()), withComments("", "what they go by"))
	before(t, p, []string{"ADD COLUMN", "nick"}, []string{"COMMENT ON COLUMN"})

	// Neither SQL Server's nor Oracle's introspection reads comments back,
	// so neither diff writes one it would never see again.
	for _, d := range []Dialect{MSSQL, Oracle} {
		if p := planFor(t, d, withComments("", ""), withComments("a", "b")); !p.Empty() {
			t.Errorf("%s: wrote a comment it cannot read back:\n%s", d, p.SQL())
		}
	}
}

func TestExclusionConstraintsAreCompared(t *testing.T) {
	mk := func(op string) *schema.Schema {
		tb := mtbl("bookings", mcol("id", "int8", true), mcol("room", "int4", true), mcol("during", "tstzrange", true))
		if op != "" {
			tb.Excludes = []*schema.Exclude{{Name: "ex_bookings_room_during", Parts: []schema.ExcludePart{
				{Column: "room", Operator: "="}, {Column: "during", Operator: op}}}}
		}
		return msch(tb)
	}
	p := planFor(t, Postgres, mk(""), mk("&&"))
	if got := sqls(p); len(got) != 1 || !strings.Contains(got[0], `ADD CONSTRAINT "ex_bookings_room_during" EXCLUDE USING gist`) {
		t.Errorf("add:\n%s", p.SQL())
	}
	p = planFor(t, Postgres, mk("&&"), mk("-|-"))
	before(t, p, []string{"DROP CONSTRAINT", "ex_bookings"}, []string{"ADD CONSTRAINT", "ex_bookings"})
	p = planFor(t, Postgres, mk("&&"), mk(""))
	if len(p.Changes) != 1 || !p.Changes[0].Destructive || p.Changes[0].Object != ObjectExclude {
		t.Errorf("drop:\n%s", p.SQL())
	}
	if p := planFor(t, Postgres, mk("&&"), mk("&&")); !p.Empty() {
		t.Errorf("unchanged:\n%s", p.SQL())
	}
}

func generated(expr string) *schema.Schema {
	return msch(people(&schema.Column{Name: "domain", Type: schema.Type{Name: "text"}, Generated: expr}))
}

func TestGeneratedColumnChanges(t *testing.T) {
	for _, d := range dialects {
		// A new expression: drop and add, losing nothing the expression
		// does not recompute.
		p := planFor(t, d, generated("split_part(email, '@', 2)"), generated("lower(split_part(email, '@', 2))"))
		before(t, p, []string{"DROP COLUMN", "domain"}, []string{"ADD", "domain", "lower"})
		if p.Destructive() {
			t.Errorf("%s: a generated column's values are the expression's:\n%s", d, p.SQL())
		}
		// A plain column becoming generated loses what it held.
		p = planFor(t, d, generated(""), generated("lower(email)"))
		if !p.Destructive() {
			t.Errorf("%s: replacing stored values is destructive:\n%s", d, p.SQL())
		}
	}
	// Stopping being generated keeps the values on PostgreSQL ...
	p := planFor(t, Postgres, generated("lower(email)"), generated(""))
	if got := sqls(p); len(got) != 1 || got[0] != `ALTER TABLE "people" ALTER COLUMN "domain" DROP EXPRESSION;` {
		t.Errorf("postgres:\n%s", p.SQL())
	}
	// ... and cannot elsewhere.
	for _, d := range []Dialect{MSSQL, Oracle} {
		if p := planFor(t, d, generated("lower(email)"), generated("")); !p.Destructive() {
			t.Errorf("%s: losing computed values is destructive:\n%s", d, p.SQL())
		}
	}
}

// An index on a generated column goes with the column when it is dropped and
// added again, so it is rebuilt with it.
func TestRegeneratedColumnRebuildsItsIndex(t *testing.T) {
	mk := func(expr string) *schema.Schema {
		s := generated(expr)
		s.Tables[0].Indexes = []*schema.Index{{Name: "ix_people_domain", Columns: []schema.IndexColumn{{Name: "domain"}}}}
		return s
	}
	for _, d := range dialects {
		p := planFor(t, d, mk("lower(email)"), mk("upper(email)"))
		before(t, p, []string{"DROP INDEX", "ix_people_domain"}, []string{"DROP COLUMN", "domain"})
		before(t, p, []string{"ADD", "domain"}, []string{"CREATE INDEX", "ix_people_domain"})
	}
}

func counter(identity, serial bool) *schema.Schema {
	return msch(people(&schema.Column{Name: "n", Type: schema.Type{Name: "int8"}, NotNull: true,
		Identity: identity, Serial: serial}))
}

func TestIdentityAndSerialChanges(t *testing.T) {
	p := planFor(t, Postgres, counter(false, false), counter(true, false))
	mustAt(t, p, "ADD GENERATED BY DEFAULT AS IDENTITY")
	before(t, p, []string{"ADD GENERATED"}, []string{"setval", "pg_get_serial_sequence"})
	p = planFor(t, Postgres, counter(true, false), counter(false, false))
	if got := sqls(p); len(got) != 1 || !strings.HasSuffix(got[0], "DROP IDENTITY;") {
		t.Errorf("postgres identity drop:\n%s", p.SQL())
	}
	p = planFor(t, Postgres, counter(false, false), counter(false, true))
	before(t, p, []string{"CREATE SEQUENCE", `"people_n_seq" AS bigint OWNED BY`}, []string{"SET DEFAULT nextval"})
	mustAt(t, p, "setval", "people_n_seq")
	p = planFor(t, Postgres, counter(false, true), counter(true, false))
	before(t, p, []string{"DROP SEQUENCE"}, []string{"ADD GENERATED"})

	// SQL Server cannot change IDENTITY at all; Oracle can drop one only.
	for _, c := range []struct {
		d        Dialect
		from, to *schema.Schema
		stmt     string
	}{
		{MSSQL, counter(false, false), counter(true, false), ""},
		{MSSQL, counter(true, false), counter(false, false), ""},
		{Oracle, counter(false, false), counter(true, false), ""},
		{Oracle, counter(true, false), counter(false, false), `ALTER TABLE "people" MODIFY ("n" DROP IDENTITY)`},
	} {
		p := planFor(t, c.d, c.from, c.to)
		if c.stmt != "" {
			if got := sqls(p); len(got) != 1 || got[0] != c.stmt {
				t.Errorf("%s:\n%s", c.d, p.SQL())
			}
			continue
		}
		if len(p.Changes) != 1 || !p.Changes[0].Destructive || !strings.HasPrefix(p.Changes[0].SQL, "-- cannot") {
			t.Errorf("%s: an identity change the server has no ALTER for must say so:\n%s", c.d, p.SQL())
		}
	}
}

func TestEnumLabelGoesWhereTheModelPutsIt(t *testing.T) {
	mk := func(labels ...string) *schema.Schema {
		return &schema.Schema{Enums: []*schema.Enum{{Name: "status", Labels: labels}}}
	}
	cases := []struct {
		from, to []string
		want     []string
	}{
		{[]string{"a", "c"}, []string{"a", "b", "c"}, []string{`ALTER TYPE "status" ADD VALUE 'b' AFTER 'a';`}},
		{[]string{"b", "c"}, []string{"a", "b", "c"}, []string{`ALTER TYPE "status" ADD VALUE 'a' BEFORE 'b';`}},
		{[]string{"a"}, []string{"a", "b"}, []string{`ALTER TYPE "status" ADD VALUE 'b';`}},
		{[]string{"c"}, []string{"a", "b", "c"}, []string{
			`ALTER TYPE "status" ADD VALUE 'a' BEFORE 'c';`, `ALTER TYPE "status" ADD VALUE 'b' AFTER 'a';`}},
	}
	for _, c := range cases {
		p := Diff(mk(c.from...), mk(c.to...))
		if got := sqls(p); strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%v -> %v:\n%s", c.from, c.to, p.SQL())
		}
	}
	p := Diff(mk("a", "b"), mk("b", "a"))
	if len(p.Changes) != 1 || !p.Changes[0].Destructive || !strings.Contains(p.Changes[0].SQL, "cannot reorder") {
		t.Errorf("reorder:\n%s", p.SQL())
	}
}

// SQL Server's ALTER COLUMN refuses a column an index, a key or a DEFAULT is
// bound to (error 5074), so each is taken off and put back around it.
func TestMSSQLAlterColumnFreesWhatDependsOnIt(t *testing.T) {
	mk := func(size int) *schema.Schema {
		tb := people(&schema.Column{Name: "nick", Type: schema.Type{Name: "varchar", Size: size}, Default: "'x'"})
		tb.Indexes = []*schema.Index{{Name: "ix_people_nick", Columns: []schema.IndexColumn{{Name: "nick"}}}}
		tb.Uniques = []*schema.Unique{{Name: "uq_people_nick", Columns: []string{"nick"}}}
		return msch(tb)
	}
	p := planFor(t, MSSQL, mk(40), mk(80))
	alter := mustAt(t, p, "ALTER COLUMN [nick]")
	for _, obj := range []string{"DROP INDEX [ix_people_nick]", "DROP CONSTRAINT [uq_people_nick]", "sys.default_constraints"} {
		if i := mustAt(t, p, obj); i > alter {
			t.Errorf("%s must come before the alter:\n%s", obj, p.SQL())
		}
	}
	for _, obj := range []string{"CREATE INDEX [ix_people_nick]", "ADD CONSTRAINT [uq_people_nick]", "DEFAULT ('x') FOR [nick]"} {
		if i := mustAt(t, p, obj); i < alter {
			t.Errorf("%s must come after the alter:\n%s", obj, p.SQL())
		}
	}
	// PostgreSQL rebuilds the index itself, and keeps the default.
	p = planFor(t, Postgres, mk(40), mk(80))
	if got := sqls(p); len(got) != 1 {
		t.Errorf("postgres widens in one step:\n%s", p.SQL())
	}
}

func TestMSSQLDropColumnDropsItsDefaultFirst(t *testing.T) {
	from := people(&schema.Column{Name: "nick", Type: schema.Type{Name: "text"}, Default: "'x'"})
	p := planFor(t, MSSQL, msch(from), msch(people()))
	s := p.Changes[0].SQL
	if i, j := strings.Index(s, "sys.default_constraints"), strings.Index(s, "DROP COLUMN [nick]"); i < 0 || j < i {
		t.Errorf("the default must go before the column:\n%s", s)
	}
}
