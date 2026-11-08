// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package migratesum_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/storm/migrate/sum"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	write(t, d, "0001_init.up.sql", "CREATE TABLE a (id int);\n")
	write(t, d, "0001_init.down.sql", "DROP TABLE a;\n")
	write(t, d, "0002_b.up.sql", "CREATE TABLE b (id int);\n")
	if _, err := migratesum.Write(d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCheck_CleanDirectoryPasses(t *testing.T) {
	if err := migratesum.Check(dir(t)); err != nil {
		t.Fatal(err)
	}
}

func TestCheck_NoSumIsReportedAsSuch(t *testing.T) {
	d := t.TempDir()
	write(t, d, "0001_init.up.sql", "SELECT 1;")
	if err := migratesum.Check(d); !errors.Is(err, migratesum.ErrNoSum) {
		t.Fatalf("got %v, want ErrNoSum", err)
	}
}

func TestCheck_EditedMigrationFails(t *testing.T) {
	d := dir(t)
	write(t, d, "0001_init.up.sql", "CREATE TABLE a (id bigint);\n")
	var e *migratesum.Error
	if err := migratesum.Check(d); !errors.As(err, &e) || len(e.Modified) != 1 || e.Modified[0] != "0001_init.up.sql" {
		t.Fatalf("got %v", err)
	}
}

func TestCheck_AddedAndRemovedFail(t *testing.T) {
	d := dir(t)
	write(t, d, "0003_c.up.sql", "SELECT 1;")
	if err := os.Remove(filepath.Join(d, "0002_b.up.sql")); err != nil {
		t.Fatal(err)
	}
	var e *migratesum.Error
	if err := migratesum.Check(d); !errors.As(err, &e) ||
		len(e.Added) != 1 || len(e.Removed) != 1 {
		t.Fatalf("got %v", err)
	}
}

// Two branches each ran `storm diff`: two version 3s. Reported with or
// without a sum, since the sum may have been "resolved" by hand.
func TestCheck_DuplicateVersionsFail(t *testing.T) {
	d := dir(t)
	write(t, d, "0003_tags.up.sql", "SELECT 1;")
	write(t, d, "0003_status.up.sql", "SELECT 2;")
	if _, err := migratesum.Write(d); err != nil {
		t.Fatal(err)
	}
	var e *migratesum.Error
	if err := migratesum.Check(d); !errors.As(err, &e) || len(e.Duplicates) != 2 {
		t.Fatalf("got %v", err)
	}
	if err := os.Remove(filepath.Join(d, migratesum.File)); err != nil {
		t.Fatal(err)
	}
	if err := migratesum.Check(d); !errors.As(err, &e) || len(e.Duplicates) != 2 {
		t.Fatalf("without a sum: got %v", err)
	}
}

// The branch race again, from git's side: both branches rewrite line 1, so
// their merge conflicts there, and a sum left with the markers in is refused.
func TestWrite_TwoBranchesDisagreeOnTheFirstLine(t *testing.T) {
	a, b := dir(t), dir(t)
	write(t, a, "0003_tags.up.sql", "SELECT 1;")
	write(t, b, "0003_status.up.sql", "SELECT 2;")
	sa, _ := migratesum.Write(a)
	sb, _ := migratesum.Write(b)
	if sa.Total == sb.Total {
		t.Fatal("two different directories have one total")
	}
	write(t, a, migratesum.File, "<<<<<<< HEAD\n"+sa.String())
	if err := migratesum.Check(a); err == nil || !strings.Contains(err.Error(), "merge conflict") {
		t.Fatalf("a sum with conflict markers was accepted: %v", err)
	}
}

func TestCheck_HandEditedSumIsCorrupt(t *testing.T) {
	d := dir(t)
	write(t, d, "0002_b.up.sql", "CREATE TABLE b (id bigint);\n")
	s, err := migratesum.Compute(d)
	if err != nil {
		t.Fatal(err)
	}
	// The entries updated to match, the total left alone.
	recorded, _ := migratesum.Read(d)
	s.Total = recorded.Total
	write(t, d, migratesum.File, s.String())
	var e *migratesum.Error
	if err := migratesum.Check(d); !errors.As(err, &e) || !e.Corrupt {
		t.Fatalf("got %v", err)
	}
}

func TestCompute_LineEndingsAreNotAnEdit(t *testing.T) {
	d := dir(t)
	write(t, d, "0002_b.up.sql", "CREATE TABLE b (id int);\r\n")
	if err := migratesum.Check(d); err != nil {
		t.Fatalf("a CRLF checkout read as an edit: %v", err)
	}
}

func TestParse_RoundTrips(t *testing.T) {
	s, err := migratesum.Compute(dir(t))
	if err != nil {
		t.Fatal(err)
	}
	back, err := migratesum.Parse([]byte(s.String()))
	if err != nil {
		t.Fatal(err)
	}
	if back.String() != s.String() {
		t.Fatalf("round trip changed the file:\n%s\nvs\n%s", back, s)
	}
}
