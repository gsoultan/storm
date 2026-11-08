// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package codegenmongo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/storm/schema"
)

// related: an org has users, a user may belong to an org (a nullable key)
// and has one profile.
func related() *schema.Schema {
	uuid := schema.Type{Name: schema.TypeUUID}
	id := func() *schema.Column {
		return &schema.Column{Name: "id", Type: uuid, NotNull: true, Default: "gen_random_uuid()"}
	}
	return &schema.Schema{Tables: []*schema.Table{
		{Name: "orgs", GoName: "Org", PrimaryKey: []string{"id"}, Columns: []*schema.Column{id(),
			{Name: "code", Type: schema.Type{Name: schema.TypeText}, NotNull: true}},
			Relations: []*schema.Relation{{Field: "Users", Target: "users", ToMany: true, Column: "org_code"}},
			Plans: []*schema.Plan{{Name: "Tree", Fields: []schema.PlanField{{Field: "Users",
				Nested: []schema.PlanField{{Field: "Profile"}, {Field: "Org"}}}}}}},
		{Name: "users", GoName: "User", PrimaryKey: []string{"id"}, Columns: []*schema.Column{id(),
			{Name: "org_code", Type: schema.Type{Name: schema.TypeText}}},
			ForeignKeys: []*schema.ForeignKey{{Columns: []string{"org_code"}, RefTable: "orgs", RefColumns: []string{"code"}, Document: "reference"}},
			Relations: []*schema.Relation{
				{Field: "Org", Target: "orgs", Owner: true, Column: "org_code", Nullable: true, Document: "reference"},
				{Field: "Profile", Target: "profiles", Column: "user_id", Nullable: true}}},
		{Name: "profiles", GoName: "Profile", PrimaryKey: []string{"id"}, Columns: []*schema.Column{id(),
			{Name: "user_id", Type: uuid, NotNull: true}},
			Relations: []*schema.Relation{{Field: "User", Target: "users", Owner: true, Column: "user_id", Document: "reference"}}},
	}}
}

func TestPlansGenerateAndCompile(t *testing.T) {
	files, err := Package(related(), Options{Import: "github.com/gsoultan/storm", Package: "ctx",
		PackageImport: "github.com/gsoultan/storm/internal/x"})
	if err != nil {
		t.Fatal(err)
	}
	src := string(files["ctx.gen.go"])
	for _, want := range []string{
		"type OrgTreeUsersRow struct {",
		"Profile *profile.Row",
		"Org     *org.Row",
		"Users []OrgTreeUsersRow",
		"func UserWithOrg() UserWithOrgQuery",
		"if !out[i].OrgCode.Valid {",
		"user.OrgCode.In(keys...)",
		"org.Code.In(keys...)).Limit(int64(len(keys)))",
		"It costs 4 round trips",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("lacks %s", want)
		}
	}
	if testing.Short() {
		t.Skip("-short: not building the generated packages")
	}
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	base := "mgplans" + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })
	files, err = Package(related(), Options{Import: "github.com/gsoultan/storm", Package: base,
		PackageImport: "github.com/gsoultan/storm/internal/" + base})
	if err != nil {
		t.Fatal(err)
	}
	for rel, b := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "vet", "./internal/"+base+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the generated packages do not build:\n%s", out)
	}
}

func TestPlansRefuse(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*schema.Schema)
		want string
	}{
		"an unknown field": {func(s *schema.Schema) {
			s.Tables[0].Plans[0].Fields[0].Field = "Nope"
		}, "not a relation"},
		"a join table": {func(s *schema.Schema) {
			s.Tables[0].Plans = append(s.Tables[0].Plans, &schema.Plan{Name: "Linked", Fields: []schema.PlanField{{Field: "Tags"}}})
			s.Tables[0].Relations = append(s.Tables[0].Relations, &schema.Relation{Field: "Tags", Target: "users", Link: "x"})
		}, "join table"},
		"a composite target": {func(s *schema.Schema) {
			s.Tables[0].PrimaryKey = []string{"id", "code"}
			s.Tables[1].ForeignKeys = nil
		}, "composite key"},
		"a key that cannot be matched": {func(s *schema.Schema) {
			s.Tables[1].Columns[1].Type = schema.Type{Name: schema.TypeInt4}
		}, "cannot be matched"},
		"a name twice": {func(s *schema.Schema) {
			s.Tables[0].Plans[0].Name = "WithUsers"
		}, "both named OrgWithUsers"},
		"a nested unknown field": {func(s *schema.Schema) {
			s.Tables[0].Plans[0].Fields[0].Nested[0].Field = "Nope"
		}, "not a relation"},
	} {
		s := related()
		c.edit(s)
		_, err := plans(s, []string{"orgs", "users", "profiles"})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := plans(related(), []string{"orgs", "profiles"}); err == nil || !strings.Contains(err.Error(), "not generated with it") {
		t.Errorf("a plan across the generation: %v", err)
	}
	if _, err := Package(related(), Options{Import: "x", Package: "ctx"}); err == nil {
		t.Error("Package without PackageImport")
	}
	if _, err := Package(related(), Options{Import: "x", Package: "ctx", PackageImport: "y", Only: []string{"orgs", "profiles"}}); err == nil {
		t.Error("a named plan over a table not generated")
	}
}
