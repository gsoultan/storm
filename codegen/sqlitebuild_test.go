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
	"time"

	"github.com/gsoultan/storm/v2"
	"github.com/gsoultan/storm/v2/codegen"
)

// The SQLite back end's compile gate, as TestMySQLGeneratedPackageCompiles is
// MySQL's: every portable type, a relation both ways, and the package must
// BUILD and carry SQLite's SQL. That the SQL also RUNS is
// internal/sqlitecheck's to say, against a real database.

type liteOrg struct {
	storm.Model
	Name    string
	Members []liteMember
}

func (o *liteOrg) Schema(t *storm.Table) { t.Col(&o.Name).Size(120); t.Unique(&o.Name) }

type liteMember struct {
	storm.Model
	Org     liteOrg
	Email   string
	Age     *int16
	Active  bool
	Balance storm.Decimal
	Small   int16
	Big     int64
	Double  float64
	Blob    []byte
	Stamp   time.Time
	Day     time.Time
	Clock   storm.TimeOfDay
	Doc     storm.JSON
	OptTime *time.Time
	OptDoc  *storm.JSON
}

func (m *liteMember) Schema(t *storm.Table) {
	t.Col(&m.Email).Size(320)
	t.Col(&m.Balance).Numeric(15, 4)
	t.Col(&m.Day).Date()
	t.Col(&m.Org).OnDelete(storm.Cascade)
	t.Index(&m.Email)
}

func TestSQLiteGeneratedPackageCompiles(t *testing.T) {
	s, err := storm.Build(&liteOrg{}, &liteMember{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "internal", "litebuild"+strconv.Itoa(os.Getpid()))
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: dir, Import: "github.com/gsoultan/storm/v2", Dialect: codegen.DialectSQLite,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated; the test would assert nothing")
	}
	var all strings.Builder
	for rel, src := range files {
		all.Write(src)
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, sql := range sqlConstants(all.String()) {
		if strings.Contains(sql, "$1") {
			t.Errorf("a PostgreSQL placeholder reached SQLite SQL:\n  %s", sql)
		}
		if strings.Contains(sql, "::") {
			t.Errorf("a PostgreSQL cast reached SQLite SQL:\n  %s", sql)
		}
	}
	if !strings.Contains(all.String(), `INSERT INTO "lite_members"`) {
		t.Error("no insert was emitted; the fixture or the extractor is wrong")
	}

	cmd := exec.Command("go", "build", "./internal/"+filepath.Base(dir)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("SQLite-dialect generated code does not compile:\n%s", out)
	}
}
