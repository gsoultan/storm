// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package codegen

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/v2/compile/pgsql"
	"github.com/gsoultan/storm/v2/schema"
)

// The SQLite lowering's adapters — the closures that turn the seam's strings
// into compile/sqlite's typed arguments — each produce SQLite's SQL, not
// PostgreSQL's. A closure wired to the pgsql twin by a copy-paste would pass
// every test that only generates a package, since the two read alike.
func TestTheSQLiteLoweringAdaptsToCompileSQLite(t *testing.T) {
	l := loweringFor(DialectSQLite)
	if DialectSQLite.String() != "sqlite" || l.name != "sqlite" {
		t.Fatalf("name: %q %q", DialectSQLite.String(), l.name)
	}
	if !l.KeysAreClientSide || l.RecursiveMaxDepth("x") != 0 {
		t.Error("SQLite generates no uuid and bounds no recursion by width")
	}
	const live = `"t"."deleted_at" IS NULL`
	cols := []string{"id", "parent_id"}
	out := map[string]string{
		"Recursive":     l.Recursive("t", cols, "id", "parent_id", "BLOB", 1, live),
		"TopNWindow":    l.TopNWindow("t", cols, "parent_id", "BLOB", live),
		"TopNLateral":   l.TopNLateral("t", cols, "parent_id", "BLOB", live),
		"ExistsFrag":    l.ExistsFrag("c", "p_id", "p", "id", live),
		"NotExistsFrag": l.NotExistsFrag("c", "p_id", "p", "id", live),
		"ExistsOpen":    l.ExistsOpen("c", "p_id", "p", "id", live),
		"NotExistsOpen": l.NotExistsOpen("c", "p_id", "p", "id", live),
		"ExistsOnOpen":  l.ExistsOnOpen("c", []string{"a", "b"}, "p", []string{"a", "b"}, live),
		"NotExistsOn":   l.NotExistsOnOpen("c", []string{"a", "b"}, "p", []string{"a", "b"}, live),
		"LiveFor":       l.LiveFor("t", "deleted_at"),
		"Upsert":        l.Upsert.Spec([]pgsql.ConflictKey{{Name: "email"}}, ""),
	}
	ins, err := l.InsertStmt("t", []string{"a"}, []string{"id"})
	if err != nil {
		t.Fatal(err)
	}
	out["InsertStmt"] = ins
	u, err := l.UnionSuffix(&schema.Union{})
	if err != nil {
		t.Fatal(err)
	}
	_ = u
	if _, _, ok := l.Frag("In", `"t"."a"`, nil); ok {
		t.Error("In without the column must be refused: the element's form depends on its type")
	}
	if a, _, ok := l.Frag("In", `"t"."a"`, &schema.Column{Name: "a", Type: schema.Type{Name: schema.TypeInt8}}); !ok || !strings.Contains(a, "json_each") {
		t.Errorf("In: %q %v", a, ok)
	}
	for name, sql := range out {
		if sql == "" {
			t.Errorf("%s produced nothing", name)
		}
		if strings.Contains(sql, "$1") || strings.Contains(sql, "::") || strings.Contains(sql, "LATERAL") {
			t.Errorf("%s produced PostgreSQL: %s", name, sql)
		}
	}
}
