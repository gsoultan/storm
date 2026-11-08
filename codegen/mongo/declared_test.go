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

// declared is related() with a join, two aggregations and a union.
func declared() *schema.Schema {
	s := related()
	users := s.Tables[1]
	users.Columns = append(users.Columns,
		&schema.Column{Name: "score", Type: schema.Type{Name: schema.TypeFloat8}},
		&schema.Column{Name: "n", Type: schema.Type{Name: schema.TypeInt4}, NotNull: true})
	users.SoftDelete = ""
	col := func(tbl, c string, ty schema.Type) schema.Expr {
		return schema.Expr{Kind: schema.ExprCol, Tbl: tbl, Col: c, Type: ty}
	}
	text := schema.Type{Name: schema.TypeText}
	uuid := schema.Type{Name: schema.TypeUUID}
	users.Joins = []*schema.Join{{Name: "WithOrg",
		Tables: []schema.JoinTable{{Kind: schema.JoinLeft, Table: "orgs", Alias: "orgs",
			On: schema.Cond{Kind: schema.CondCmp, Op: schema.OpEq, Left: col("users", "org_code", text), Right: col("orgs", "code", text)}}},
		Select: []schema.JoinCol{
			{Expr: col("users", "id", uuid), As: "ID", Type: uuid},
			{Expr: col("orgs", "code", text), As: "Code", Type: text, Nullable: true},
		},
		OrderBy: []schema.JoinOrder{{Expr: col("orgs", "code", text), Desc: true}},
	}}
	agg := func(fn schema.AggFunc, arg schema.Expr, ty schema.Type, nullable bool) schema.Expr {
		return schema.Expr{Kind: schema.ExprAgg, Fn: string(fn), Args: []schema.Expr{arg}, Type: ty, Nullable: nullable}
	}
	f8 := schema.Type{Name: schema.TypeFloat8}
	users.Aggregates = []*schema.Aggregate{
		{Name: "ByOrg", AcrossDeleted: true,
			By:      []schema.GroupTerm{{Expr: col("", "org_code", text), As: "OrgCode"}},
			OrderBy: []schema.AggOrder{{As: "Avg", Desc: true}},
			Terms: []schema.AggregateTerm{
				{Expr: agg(schema.AggAvg, col("", "score", f8), f8, true), As: "Avg"},
				{Expr: agg(schema.AggSum, col("", "n", schema.Type{Name: schema.TypeInt4}), schema.Type{Name: schema.TypeInt8}, true), As: "N"},
			}},
		{Name: "Total", Terms: []schema.AggregateTerm{
			{Expr: agg(schema.AggMax, col("", "score", f8), f8, true), As: "Best"}}},
	}
	s.Unions = []*schema.Union{{Name: "Names",
		Params: []schema.Param{{Name: "Code", Type: text}},
		Cols:   []schema.UnionCol{{As: "Name", Type: text, Nullable: true}, {As: "Kind", Type: text}},
		Branches: []schema.UnionBranch{
			{Table: "orgs", Exprs: []schema.Expr{col("", "code", text), {Kind: schema.ExprLit, Lit: schema.Literal{Kind: schema.TypeText, S: "org"}}},
				Where: &schema.Cond{Kind: schema.CondCmp, Op: schema.OpEq, Left: col("", "code", text), Right: schema.Expr{Kind: schema.ExprParam, Type: text}}},
			{Table: "users", Exprs: []schema.Expr{col("", "org_code", text), {Kind: schema.ExprLit, Lit: schema.Literal{Kind: schema.TypeText, S: "user"}}}},
		},
		OrderBy:  []schema.UnionOrder{{Col: "Name"}},
		Distinct: true,
	}}
	return s
}

func TestDeclaredReadsGenerateAndCompile(t *testing.T) {
	files, err := Package(declared(), Options{Import: "github.com/gsoultan/storm", Package: "ctx",
		PackageImport: "github.com/gsoultan/storm/internal/x"})
	if err != nil {
		t.Fatal(err)
	}
	src := string(files["user/user.gen.go"]) + string(files["ctx.gen.go"])
	for _, want := range []string{
		"type WithOrgRow struct {", "Code runtime.Null[string]", "func (q Query) AllWithOrg(",
		`\"preserveNullAndEmptyArrays\":true`,
		"type ByOrgRow struct {", `{\"$avg\":`, "filtered(byOrgCache, 1,",
		"type TotalRow struct {", "out = append(out, TotalRow{})",
		"type NamesRow struct {", "func Names(ctx context.Context, ex runtime.Executor, code string, n int64)",
		`{\"$unionWith\":{\"coll\":\"users\"`, `{\"$group\":{\"_id\":{\"Name\":\"$Name\",\"Kind\":\"$Kind\"}}}`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("lacks %s", want)
		}
	}
	if _, err := Package(declared(), Options{Import: "x"}); err == nil || !strings.Contains(err.Error(), "Options.Package") {
		t.Errorf("a union without the context package: %v", err)
	}
	if _, err := File(nil, declared().Tables[1], "user", "x"); err == nil {
		t.Error("a join needs the whole model")
	}
	if testing.Short() {
		t.Skip("-short: not building the generated packages")
	}
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	base := "mgdecl" + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })
	files, err = Package(declared(), Options{Import: "github.com/gsoultan/storm", Package: base,
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
