// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/storm/v2/codegen"
	"github.com/gsoultan/storm/v2/schema"
)

// An imported table with a column storm has no Go type for. The column is
// omitted from the struct, and every declaration that named it — an index
// over it, a unique on it — named a field that was not there, so the model did
// not compile. gateon's audit_logs(action, timestamp) index did that while
// timestamp was unsupported; xml stands in for any type that still is.
func omittedSchema() *schema.Schema {
	return &schema.Schema{Tables: []*schema.Table{{
		Name: "documents", GoName: "Document",
		Columns: []*schema.Column{
			{Name: "id", Type: schema.Type{Name: schema.TypeInt8}, NotNull: true},
			{Name: "kind", Type: schema.Type{Name: schema.TypeText}, NotNull: true},
			{Name: "body", Type: schema.Type{Name: "xml"}},
			{Name: "seen", Type: schema.Type{Name: schema.TypeTimestamp}, NotNull: true},
			{Name: "day", Type: schema.Type{Name: schema.TypeDate}},
		},
		PrimaryKey: []string{"id"},
		Indexes: []*schema.Index{
			{Name: "ix_documents_kind_body", Columns: []schema.IndexColumn{{Name: "kind"}, {Name: "body"}}},
			{Name: "ix_documents_kind", Columns: []schema.IndexColumn{{Name: "kind"}}},
		},
		Uniques: []*schema.Unique{{Name: "documents_body_key", Columns: []string{"body"}}},
	}}}
}

func TestAnImportedModelWithAnOmittedColumnCompiles(t *testing.T) {
	src, err := codegen.Model(omittedSchema(), codegen.ModelOptions{Package: "model"})
	if err != nil {
		t.Fatalf("imported model does not parse: %v\n%s", err, src)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "internal", "modelomit"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "model.go"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "./internal/"+filepath.Base(dir)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("imported model does not compile:\n%s\n--- model ---\n%s", out, src)
	}
	code := string(src)
	// What could be carried still is; what could not is listed, not dropped.
	for _, want := range []string{
		`t.Index(&m.Kind).Named("ix_documents_kind")`,
		"index ix_documents_kind_body on documents",
		"unique documents_body_key on documents",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("the model has no %q:\n%s", want, code)
		}
	}
}

// A timestamp and a date are both time.Time in Go, and a time.Time field means
// timestamptz unless the model says otherwise. Imported without saying so, the
// first diff against the source database retyped both.
func TestImportCarriesTimestampAndDate(t *testing.T) {
	src, err := codegen.Model(omittedSchema(), codegen.ModelOptions{Package: "model"})
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	for _, want := range []string{"t.Col(&m.Seen).WithoutTimeZone()", "t.Col(&m.Day).Date()"} {
		if !strings.Contains(code, want) {
			t.Errorf("the model has no %q:\n%s", want, code)
		}
	}
	if strings.Contains(code, "is timestamp, which storm has no Go type for") {
		t.Errorf("a timestamp column is still reported as omitted:\n%s", code)
	}
}
