// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package codegen_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm/v2"
	"github.com/gsoultan/storm/v2/codegen"
	"github.com/gsoultan/storm/v2/schema"
)

// A timestamp WITHOUT a time zone. Through v2.1.0 storm had no Go kind for
// it: `storm import` omitted every such column, and a NOT NULL one with no
// default refused the whole package ("storm cannot bind that type yet —
// every INSERT would fail"), because the generator could not put the column in
// an INSERT. Hand-written PostgreSQL schemas declare `timestamp` far more often
// than `timestamptz`, so an adopter with an existing database hit it on nearly
// every table.

type tsEvent struct {
	ID       int64
	Name     string
	Happened time.Time
	Settled  *time.Time
}

func (e *tsEvent) Schema(t *storm.Table) {
	t.PrimaryKey(&e.ID)
	t.Col(&e.ID).Identity()
	t.Col(&e.Happened).WithoutTimeZone()
	t.Col(&e.Settled).WithoutTimeZone()
}

func buildTimestamp(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := storm.Build(&tsEvent{})
	if err != nil {
		t.Fatalf("a model with timestamp columns did not build: %v", err)
	}
	return s
}

func TestWithoutTimeZoneDeclaresATimestamp(t *testing.T) {
	s := buildTimestamp(t)
	tab := s.Tables[0]
	for _, name := range []string{"happened", "settled"} {
		c := tab.Column(name)
		if c == nil {
			t.Fatalf("no column %s", name)
		}
		if c.Type.Name != schema.TypeTimestamp {
			t.Errorf("%s is %s, want timestamp", name, c.Type.Name)
		}
	}
}

func TestWithoutTimeZoneRefusesANonTimeField(t *testing.T) {
	_, err := storm.Build(&tsBadModel{})
	if err == nil || !strings.Contains(err.Error(), "WithoutTimeZone") {
		t.Fatalf("WithoutTimeZone on a string field: err = %v, want a refusal naming it", err)
	}
}

type tsBadModel struct {
	ID   int64
	Name string
}

func (m *tsBadModel) Schema(t *storm.Table) {
	t.PrimaryKey(&m.ID)
	t.Col(&m.Name).WithoutTimeZone()
}

// The package generates on every SQL target, and the column is a time.Time in
// the Row and in the insert — not omitted, and not an any.
func TestATimestampColumnIsReadAndWritten(t *testing.T) {
	s := buildTimestamp(t)
	for _, d := range []codegen.Dialect{
		codegen.DialectPostgres, codegen.DialectMySQL, codegen.DialectMariaDB,
		codegen.DialectMSSQL, codegen.DialectOracle, codegen.DialectSQLite,
	} {
		files, err := codegen.Package(s, codegen.PackageOptions{
			Dir: "store", Import: "github.com/gsoultan/storm/v2", Dialect: d,
		})
		if err != nil {
			t.Fatalf("%v: a NOT NULL timestamp with no default refused the package: %v", d, err)
		}
		var src strings.Builder
		for rel, b := range files {
			if strings.Contains(rel, "/") {
				src.Write(b)
			}
		}
		code := src.String()
		for _, want := range []string{"Happened time.Time", "Settled  runtime.Null[time.Time]"} {
			if !strings.Contains(code, want) {
				t.Errorf("%v: the Row has no %q", d, want)
			}
		}
	}
}

// Against a real server, in a session whose zone is NOT UTC — the case where a
// timestamp and a timestamptz disagree. A UTC time written to a timestamp
// column comes back as the same instant, and a filter on it finds the row.
func TestATimestampRoundTripsAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s := buildTimestamp(t)
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "tslive" + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: dir, Import: "github.com/gsoultan/storm/v2",
	})
	if err != nil {
		t.Fatal(err)
	}
	var pkgDir string
	for rel, src := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(rel, ".gen.go") && strings.Contains(rel, "/") {
			pkgDir = filepath.Dir(full)
		}
	}
	if pkgDir == "" {
		t.Fatal("no per-table package was generated")
	}
	pkg := filepath.Base(pkgDir)
	src := strings.ReplaceAll(timestampLiveSrc, "PKG", pkg)
	src = strings.ReplaceAll(src, "IMPORTPATH", "github.com/gsoultan/storm/v2/internal/"+base+"/"+pkg)
	if err := os.WriteFile(filepath.Join(pkgDir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-v", "./internal/"+base+"/"+pkg+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a timestamp column does not round-trip:\n%s", out)
	}
	for _, name := range []string{"TestTimestampRoundTrips", "TestTimestampFilters", "TestNullTimestamp"} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

const timestampLiveSrc = `package PKG_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gsoultan/storm/v2"
	"github.com/gsoultan/storm/v2/compile/pgddl"
	"github.com/gsoultan/storm/v2/runtime"
	"github.com/gsoultan/storm/v2/runtime/caduceusdrv"
	ev "IMPORTPATH"
)

type tsEvent struct {
	ID       int64
	Name     string
	Happened time.Time
	Settled  *time.Time
}

func (e *tsEvent) Schema(t *storm.Table) {
	t.PrimaryKey(&e.ID)
	t.Col(&e.ID).Identity()
	t.Col(&e.Happened).WithoutTimeZone()
	t.Col(&e.Settled).WithoutTimeZone()
}

var ex runtime.Executor

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "STORM_DSN did not reach the generated package")
		os.Exit(1)
	}
	ctx := context.Background()
	s, err := storm.Build(&tsEvent{})
	must(err)
	p, err := caduceusdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_timestamp_live"
	_, err = p.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE; CREATE SCHEMA "+ns, nil)
	must(err)
	_, err = p.Exec(ctx, "SET search_path TO "+ns+"; "+pgddl.Create(s), nil)
	must(err)
	p.Close()
	// A zone far from UTC: if anything converted the value through the
	// session, the round trip would be off by hours.
	pool, err := caduceusdrv.NewPoolConfig(ctx, caduceusdrv.PoolConfig{DSN: dsn,
		RuntimeParams: map[string]string{"search_path": ns, "TimeZone": "Asia/Jakarta"}})
	must(err)
	ex = pool
	code := m.Run()
	_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE", nil)
	pool.Close()
	os.Exit(code)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func one(t *testing.T, id int64) ev.Row {
	t.Helper()
	rows, err := ev.New().Where(ev.ID.Eq(id)).All(context.Background(), ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("id %d: %d rows", id, len(rows))
	}
	return rows[0]
}

var stamp = time.Date(2026, 11, 28, 8, 30, 15, 123456000, time.UTC)

func TestTimestampRoundTrips(t *testing.T) {
	ctx := context.Background()
	settled := stamp.Add(time.Hour)
	r := &ev.Row{ID: 1, Name: "round", Happened: stamp, Settled: runtime.Null[time.Time]{V: settled, Valid: true}}
	if err := ev.Insert(ctx, ex, r); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got := one(t, r.ID)
	if !got.Happened.Equal(stamp) {
		t.Errorf("happened = %v, want %v", got.Happened, stamp)
	}
	if !got.Settled.Valid || !got.Settled.V.Equal(settled) {
		t.Errorf("settled = %+v, want %v", got.Settled, settled)
	}
	var wall string
	rows, err := ex.Query(ctx, "SELECT happened::text FROM ts_events WHERE id = $1", []any{r.ID})
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		wall = string(rows.RawValues()[0])
	}
	rows.Close()
	if wall != "2026-11-28 08:30:15.123456" {
		t.Errorf("the column holds %q, want the UTC wall clock", wall)
	}
}

func TestTimestampFilters(t *testing.T) {
	ctx := context.Background()
	r := &ev.Row{ID: 2, Name: "filter", Happened: stamp.Add(48 * time.Hour)}
	if err := ev.Insert(ctx, ex, r); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := ev.New().Where(ev.Happened.Gt(stamp.Add(24 * time.Hour))).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "filter" {
		t.Errorf("a filter on the timestamp found %+v", rows)
	}
}

func TestNullTimestamp(t *testing.T) {
	ctx := context.Background()
	r := &ev.Row{ID: 3, Name: "null", Happened: stamp}
	if err := ev.Insert(ctx, ex, r); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got := one(t, r.ID)
	if got.Settled.Valid {
		t.Errorf("an unset timestamp came back valid: %+v", got.Settled)
	}
}
`
