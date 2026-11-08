// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package migrate_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/schema"
)

// A CREATE INDEX CONCURRENTLY that fails — a duplicate under a unique index, a
// cancelled statement — leaves the index behind, marked INVALID, and the
// planner never uses it. Its definition reads exactly like the one the model
// asks for, so a diff that compares definitions alone reports "matches the
// model" over an index that does not work. The plan must rebuild it.
func TestFor_RebuildsAnIndexAFailedConcurrentBuildLeftInvalid(t *testing.T) {
	ns := "storm_online_invalid_ix"
	ctx, c := conn(t, ns)

	if _, err := c.Exec(ctx, "CREATE SCHEMA "+ns+
		"; CREATE TABLE "+ns+".users (id uuid NOT NULL, email text NOT NULL, PRIMARY KEY (id))"+
		"; INSERT INTO "+ns+".users VALUES (gen_random_uuid(), 'a'), (gen_random_uuid(), 'a')"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "CREATE UNIQUE INDEX CONCURRENTLY ux_users_email ON "+ns+".users (email)"); err == nil {
		t.Fatal("the concurrent build over a duplicate succeeded; the test cannot leave an invalid index")
	}
	if _, err := c.Exec(ctx, "DELETE FROM "+ns+".users WHERE ctid <> (SELECT min(ctid) FROM "+ns+".users)"); err != nil {
		t.Fatal(err)
	}

	want := users()
	want.Tables[0].PrimaryKey = []string{"id"}
	want.Tables[0].Indexes = []*schema.Index{{
		Name: "ux_users_email", Unique: true, Columns: []schema.IndexColumn{{Name: "email"}},
	}}
	plan, err := migrate.ForWith(ctx, c, ns, want, migrate.Options{Concurrently: true})
	if err != nil {
		t.Fatal(err)
	}
	sql := plan.SQL()
	if !strings.Contains(sql, "DROP INDEX CONCURRENTLY") || !strings.Contains(sql, "CREATE UNIQUE INDEX CONCURRENTLY") {
		t.Fatalf("the invalid index was not rebuilt; plan:\n%s", sql)
	}

	// And applying it leaves a VALID index that the next diff is quiet about.
	if _, err := migrate.Auto(ctx, c, want, migrate.AutoOptions{Schema: ns, Concurrently: true}); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := c.QueryRow(ctx, `SELECT i.indisvalid FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = ic.relnamespace WHERE n.nspname = $1 AND ic.relname = 'ux_users_email'`,
		ns).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatal("after Auto the index is still invalid")
	}
	again, err := migrate.For(ctx, c, ns, want)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("a valid index still reported as drift:\n%s", again.SQL())
	}
}

// A constraint added NOT VALID is enforced for new rows and promises nothing
// about old ones. Until it is validated it is not the constraint the model
// declares, and saying "matches" over it hides rows that break it.
func TestFor_ValidatesANotValidConstraint(t *testing.T) {
	ns := "storm_online_notvalid"
	ctx, c := conn(t, ns)

	if _, err := c.Exec(ctx, "CREATE SCHEMA "+ns+
		"; CREATE TABLE "+ns+".users (id uuid NOT NULL, email text NOT NULL, PRIMARY KEY (id))"+
		"; CREATE TABLE "+ns+".notes (id uuid NOT NULL, user_id uuid NOT NULL, n int NOT NULL, PRIMARY KEY (id))"+
		"; ALTER TABLE "+ns+".notes ADD CONSTRAINT fk_notes_user_id FOREIGN KEY (user_id) REFERENCES "+ns+".users (id) NOT VALID"+
		"; ALTER TABLE "+ns+".notes ADD CONSTRAINT ck_notes_n CHECK (n > 0) NOT VALID"); err != nil {
		t.Fatal(err)
	}
	want := sch(
		&schema.Table{Name: "users", PrimaryKey: []string{"id"},
			Columns: []*schema.Column{col("id", "uuid", true), col("email", "text", true)}},
		&schema.Table{Name: "notes", PrimaryKey: []string{"id"},
			Columns: []*schema.Column{col("id", "uuid", true), col("user_id", "uuid", true), col("n", "int4", true)},
			ForeignKeys: []*schema.ForeignKey{{Name: "fk_notes_user_id", Columns: []string{"user_id"},
				RefTable: "users", RefColumns: []string{"id"}}},
			Checks: []*schema.Check{{Name: "ck_notes_n", Expr: "n > 0"}},
		},
	)
	plan, err := migrate.For(ctx, c, ns, want)
	if err != nil {
		t.Fatal(err)
	}
	sql := plan.SQL()
	for _, w := range []string{
		`ALTER TABLE "notes" VALIDATE CONSTRAINT "fk_notes_user_id";`,
		`ALTER TABLE "notes" VALIDATE CONSTRAINT "ck_notes_n";`,
	} {
		if !strings.Contains(sql, w) {
			t.Errorf("plan lacks %s; plan:\n%s", w, sql)
		}
	}
	if strings.Contains(sql, "DROP CONSTRAINT") {
		t.Errorf("a NOT VALID constraint is validated, not dropped and re-added; plan:\n%s", sql)
	}
	if _, err := migrate.Auto(ctx, c, want, migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatal(err)
	}
	again, err := migrate.For(ctx, c, ns, want)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("validated constraints still reported as drift:\n%s", again.SQL())
	}
}

// Online mode adds a foreign key or a check to a table that already has rows
// in two steps — ADD ... NOT VALID, then VALIDATE — and a table the plan
// creates in one, since nothing is reading a table that does not exist yet.
func TestPlan_ConcurrentlyAddsConstraintsNotValidThenValidates(t *testing.T) {
	live := sch(
		&schema.Table{Name: "users", PrimaryKey: []string{"id"}, Columns: []*schema.Column{col("id", "uuid", true)}},
		&schema.Table{Name: "notes", PrimaryKey: []string{"id"},
			Columns: []*schema.Column{col("id", "uuid", true), col("user_id", "uuid", true), col("n", "int4", true)}},
	)
	want := sch(
		&schema.Table{Name: "users", PrimaryKey: []string{"id"}, Columns: []*schema.Column{col("id", "uuid", true)}},
		&schema.Table{Name: "notes", PrimaryKey: []string{"id"},
			Columns: []*schema.Column{col("id", "uuid", true), col("user_id", "uuid", true), col("n", "int4", true)},
			ForeignKeys: []*schema.ForeignKey{{Name: "fk_notes_user_id", Columns: []string{"user_id"},
				RefTable: "users", RefColumns: []string{"id"}}},
			Checks: []*schema.Check{{Name: "ck_notes_n", Expr: "n > 0"}},
		},
		&schema.Table{Name: "tags", PrimaryKey: []string{"id"},
			Columns:     []*schema.Column{col("id", "uuid", true), col("user_id", "uuid", true)},
			ForeignKeys: []*schema.ForeignKey{{Name: "fk_tags_user_id", Columns: []string{"user_id"}, RefTable: "users", RefColumns: []string{"id"}}},
		},
	)
	p := migrate.Diff(live, want).Concurrently(live)
	got := p.SQL()
	for _, w := range []string{
		`ALTER TABLE "notes" ADD CONSTRAINT "fk_notes_user_id" FOREIGN KEY ("user_id") REFERENCES "users" ("id") NOT VALID;`,
		`ALTER TABLE "notes" ADD CONSTRAINT "ck_notes_n" CHECK (n > 0) NOT VALID;`,
		`ALTER TABLE "notes" VALIDATE CONSTRAINT "fk_notes_user_id";`,
		`ALTER TABLE "notes" VALIDATE CONSTRAINT "ck_notes_n";`,
		`ALTER TABLE "tags" ADD CONSTRAINT "fk_tags_user_id" FOREIGN KEY ("user_id") REFERENCES "users" ("id");`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s\nplan:\n%s", w, got)
		}
	}
	// The validations come last, after every ADD.
	last := p.Changes[len(p.Changes)-1]
	if !last.Validates() || p.Changes[len(p.Changes)-2].Validates() == false {
		t.Errorf("the two validations are not the tail of the plan:\n%s", got)
	}
}

// End to end on a server: the split applies, leaves VALID constraints, and
// the next plan is empty.
func TestAuto_ConcurrentlyValidatesConstraintsItAdds(t *testing.T) {
	ns := "storm_online_add"
	ctx, c := conn(t, ns)
	base := sch(
		&schema.Table{Name: "users", PrimaryKey: []string{"id"}, Columns: []*schema.Column{col("id", "uuid", true)}},
		&schema.Table{Name: "notes", PrimaryKey: []string{"id"},
			Columns: []*schema.Column{col("id", "uuid", true), col("user_id", "uuid", true), col("n", "int4", true)}},
	)
	if _, err := migrate.Auto(ctx, c, base, migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+ns+".users VALUES ('00000000-0000-0000-0000-000000000001');"+
		"INSERT INTO "+ns+".notes VALUES (gen_random_uuid(), '00000000-0000-0000-0000-000000000001', 1)"); err != nil {
		t.Fatal(err)
	}
	want := sch(base.Tables[0], &schema.Table{Name: "notes", PrimaryKey: []string{"id"},
		Columns: []*schema.Column{col("id", "uuid", true), col("user_id", "uuid", true), col("n", "int4", true)},
		ForeignKeys: []*schema.ForeignKey{{Name: "fk_notes_user_id", Columns: []string{"user_id"},
			RefTable: "users", RefColumns: []string{"id"}}},
		Checks: []*schema.Check{{Name: "ck_notes_n", Expr: "n > 0"}},
	})
	res, err := migrate.Auto(ctx, c, want, migrate.AutoOptions{Schema: ns, Concurrently: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.SQL(), "NOT VALID") || !strings.Contains(res.SQL(), "VALIDATE CONSTRAINT") {
		t.Fatalf("Auto did not split the adds:\n%s", res.SQL())
	}
	var invalid int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM pg_constraint co JOIN pg_namespace n ON n.oid = co.connamespace
		WHERE n.nspname = $1 AND NOT co.convalidated`, ns).Scan(&invalid); err != nil {
		t.Fatal(err)
	}
	if invalid != 0 {
		t.Fatalf("%d constraint(s) left NOT VALID", invalid)
	}
	again, err := migrate.For(ctx, c, ns, want)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("drift after an online migration:\n%s", again.SQL())
	}
}

// A validation that fails — a row breaks the new constraint — leaves the
// constraint NOT VALID, enforced for new rows, and the next plan names it.
func TestAuto_FailedValidationLeavesAConstraintTheNextPlanValidates(t *testing.T) {
	ns := "storm_online_badrow"
	ctx, c := conn(t, ns)
	base := sch(&schema.Table{Name: "notes", PrimaryKey: []string{"id"},
		Columns: []*schema.Column{col("id", "uuid", true), col("n", "int4", true)}})
	if _, err := migrate.Auto(ctx, c, base, migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+ns+".notes VALUES (gen_random_uuid(), -1)"); err != nil {
		t.Fatal(err)
	}
	want := sch(&schema.Table{Name: "notes", PrimaryKey: []string{"id"},
		Columns: []*schema.Column{col("id", "uuid", true), col("n", "int4", true)},
		Checks:  []*schema.Check{{Name: "ck_notes_n", Expr: "n > 0"}}})
	if _, err := migrate.Auto(ctx, c, want, migrate.AutoOptions{Schema: ns, Concurrently: true}); err == nil {
		t.Fatal("validation over a breaking row succeeded")
	}
	plan, err := migrate.For(ctx, c, ns, want)
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.SQL(); got != "ALTER TABLE \"notes\" VALIDATE CONSTRAINT \"ck_notes_n\";\n" {
		t.Fatalf("the next plan should be the validation alone, got:\n%s", got)
	}
}
