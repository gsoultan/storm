// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlitecheck

// The whole stack, RUN: model → generator → generated package →
// runtime/sqlitedrv → modernc.org/sqlite.
//
// Not a new fixture. The relation fixture MySQL and MariaDB run — nine models,
// a plan, a join, an aggregate, a union, recursion, many-to-many, an arc, every
// scalar type, JSON predicates — and the soft-delete fixture PostgreSQL runs,
// with the upsert against a partial unique index, are READ from the files they
// live in and pointed at SQLite. A construct added to either is run here
// without anyone remembering to.
//
// Only the setup differs, and it is replaced by name: the driver import, the
// DDL back end, and TestMain. If either fixture is restructured so a marker
// below no longer matches, this fails saying which — never silently runs less.

import (
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fixtureConst reads one string constant out of a Go file.
func fixtureConst(t *testing.T, file, name string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, sp := range g.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if n.Name != name || i >= len(vs.Values) {
					continue
				}
				v, err := constString(vs.Values[i])
				if err != nil {
					t.Fatalf("%s in %s: %v", name, file, err)
				}
				return v
			}
		}
	}
	t.Fatalf("no constant %s in %s", name, file)
	return ""
}

// constString evaluates a constant built from string literals joined with +.
func constString(e ast.Expr) (string, error) {
	switch x := e.(type) {
	case *ast.BasicLit:
		return strconv.Unquote(x.Value)
	case *ast.BinaryExpr:
		l, err := constString(x.X)
		if err != nil {
			return "", err
		}
		r, err := constString(x.Y)
		return l + r, err
	case *ast.ParenExpr:
		return constString(x.X)
	}
	return "", &parseErr{"not a string literal expression"}
}

type parseErr struct{ s string }

func (e *parseErr) Error() string { return e.s }

// fixtureDecls copies the named top-level declarations — types, the methods on
// them, and vars — out of a Go file, as source.
func fixtureDecls(t *testing.T, file string, names map[string]bool) string {
	t.Helper()
	fset := token.NewFileSet()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(fset, file, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	found := map[string]bool{}
	keep := func(n ast.Node) {
		b.Write(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
		b.WriteString("\n\n")
	}
	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.GenDecl:
			for _, sp := range x.Specs {
				switch s := sp.(type) {
				case *ast.TypeSpec:
					if names[s.Name.Name] {
						found[s.Name.Name] = true
						keep(x)
					}
				case *ast.ValueSpec:
					if len(s.Names) == 1 && names[s.Names[0].Name] {
						found[s.Names[0].Name] = true
						keep(x)
					}
				}
			}
		case *ast.FuncDecl:
			if x.Recv == nil || len(x.Recv.List) != 1 {
				continue
			}
			r := x.Recv.List[0].Type
			if st, ok := r.(*ast.StarExpr); ok {
				r = st.X
			}
			if id, ok := r.(*ast.Ident); ok && names[id.Name] {
				keep(x)
			}
		}
	}
	for n := range names {
		if !found[n] {
			t.Fatalf("the fixture no longer declares %s in %s", n, file)
		}
	}
	return b.String()
}

// replaceOnce swaps one marked piece of a fixture, and fails if it is gone.
func replaceOnce(t *testing.T, src, old, new string) string {
	t.Helper()
	if strings.Count(src, old) != 1 {
		t.Fatalf("the fixture changed: expected exactly one %q", old)
	}
	return strings.Replace(src, old, new, 1)
}

// replaceBetween swaps everything from start up to (not including) end.
func replaceBetween(t *testing.T, src, start, end, new string) string {
	t.Helper()
	i := strings.Index(src, start)
	j := strings.Index(src, end)
	if i < 0 || j < i {
		t.Fatalf("the fixture changed: cannot find %q … %q", start, end)
	}
	return src[:i] + new + src[j:]
}

// generate writes a generator program that builds the models and runs
// codegen for SQLite, runs it, and returns the directory it generated into.
func generate(t *testing.T, base, models, build, pkg string) string {
	t.Helper()
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, base)
	// STORM_SQLITE_KEEP leaves the generated code behind, to read.
	if os.Getenv("STORM_SQLITE_KEEP") == "" {
		t.Cleanup(func() { os.RemoveAll(dir) })
	}
	gen := filepath.Join(dir, "gen")
	if err := os.MkdirAll(gen, 0o755); err != nil {
		t.Fatal(err)
	}
	prog := `package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/gsoultan/storm/v2"
	"github.com/gsoultan/storm/v2/codegen"
)

var _ = time.Time{}

` + models + `
func main() {
	s, err := storm.Build(` + build + `)
	if err != nil {
		panic(err)
	}
	out := os.Args[1]
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: out, Import: "github.com/gsoultan/storm/v2", Dialect: codegen.DialectSQLite,
		Package: "` + pkg + `", PackageImport: "github.com/gsoultan/storm/v2/internal/sqlitecheck/` + base + `/` + pkg + `",
	})
	if err != nil {
		panic(err)
	}
	for rel, b := range files {
		full := filepath.Join(out, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			panic(err)
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			panic(err)
		}
	}
}
`
	formatted, err := format.Source([]byte(prog))
	if err != nil {
		t.Fatalf("the generator program does not parse: %v\n%s", err, prog)
	}
	if err := os.WriteFile(filepath.Join(gen, "main.go"), formatted, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, pkg)
	cmd := exec.Command("go", "run", "./"+base+"/gen", out)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating for sqlite:\n%s", b)
	}
	return out
}

// runGenerated runs a package's tests and requires every named one to pass.
func runGenerated(t *testing.T, rel string, mustPass []string) {
	t.Helper()
	cmd := exec.Command("go", "test", "-count=1", "-v", "-timeout", "300s", "./"+rel+"/")
	cmd.Dir, _ = filepath.Abs(".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated package does not run:\n%s", out)
	}
	// A subprocess that SKIPPED is a green test that proved nothing.
	for _, name := range mustPass {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
	if testing.Verbose() {
		t.Logf("%s", out)
	}
}

// sqliteMain is the setup both fixtures get in place of their own: a file
// database with storm's connection settings, and compile/liteddl's DDL.
const sqliteMain = `func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "storm-sqlite")
	must(err)
	db, err := sql.Open(sqlitedrv.DriverName, sqlitedrv.DSN(filepath.Join(dir, "storm.db")))
	must(err)
	ex = sqlitedrv.NewPool(db)

	s, err := storm.Build(BUILD)
	must(err)
	stmts, err := liteddl.Statements(s)
	must(err)
	for _, st := range stmts {
		if _, err := db.Exec(st); err != nil {
			panic(st + ": " + err.Error())
		}
	}
	code := m.Run()
	db.Close()
	os.RemoveAll(dir)
	os.Exit(code)
}

`

// TestGeneratedRelationsRun is the relation fixture, on SQLite.
func TestGeneratedRelationsRun(t *testing.T) {
	const fixture = "../../codegen/relationlive_src_test.go"
	models := fixtureDecls(t, "../../codegen/mariadblive_test.go", map[string]bool{
		"mdAuthor": true, "mdPost": true, "mdNode": true, "mdTag": true, "mdAttachment": true,
		"mdWide": true, "mdFollow": true, "mdEvent": true, "mdNames": true,
	})
	// The one change to the MODEL: SQLite's NUMERIC is a float64 or an int64,
	// exact to fifteen significant digits, and compile/liteddl refuses the
	// fixture's numeric(18,6) rather than store it with digits missing. The
	// widest numeric that ports, with a value that fills it.
	models = replaceOnce(t, models, "Numeric(18, 6)", "Numeric(15, 6)")
	const build = "&mdAuthor{}, &mdPost{}, &mdNode{}, &mdTag{}, &mdAttachment{}, &mdWide{}, &mdFollow{}, &mdEvent{}"
	base := "rel" + strconv.Itoa(os.Getpid())
	pkg := "rel"
	generate(t, base, models, build+", mdNames", pkg)

	src := fixtureConst(t, fixture, "relationsLiveSrc")
	src = strings.ReplaceAll(src, "PKG", pkg)
	src = strings.ReplaceAll(src, "IMPORTPATH", "github.com/gsoultan/storm/v2/internal/sqlitecheck/"+base+"/"+pkg)
	src = replaceOnce(t, src, `"github.com/gsoultan/storm/v2/compile/myddl"`, `"github.com/gsoultan/storm/v2/compile/liteddl"`)
	src = replaceOnce(t, src, `"github.com/gsoultan/storm/v2/runtime/mydrv"`,
		`"database/sql"
	"path/filepath"

	"github.com/gsoultan/storm/v2/runtime/sqlitedrv"

	_ "modernc.org/sqlite"`)
	src = replaceOnce(t, src, "Numeric(18, 6)", "Numeric(15, 6)")
	src = replaceOnce(t, src, `"-123456789012.345678"`, `"-123456789.345678"`)
	// SQLite's time of day is a clock reading, 00:00 to 24:00, as PostgreSQL's
	// is; MySQL's signed duration is that fixture's own concern.
	src = replaceOnce(t, src,
		"storm.TimeOfDay(-((30*time.Hour + 20*time.Minute + 10*time.Second + 500*time.Millisecond) / time.Microsecond))",
		"storm.TimeOfDay((23*time.Hour + 20*time.Minute + 10*time.Second + 500*time.Millisecond) / time.Microsecond)")
	src = replaceOnce(t, src, "var ex *mydrv.Pool", "var ex sqlitedrv.Pool")
	src = replaceBetween(t, src, "func TestMain(m *testing.M) {", "func must(err error) {",
		strings.Replace(sqliteMain, "BUILD", build, 1))
	src = replaceOnce(t, src, "tx, err := ex.Begin(ctx)", "tx, err := ex.StartTx(ctx)")
	// MySQL's cycle guard is a fixed-width column and refuses a deep
	// traversal; SQLite's is text with no width, so the same depth RUNS.
	src = replaceBetween(t, src, "	// ...and so is one deeper than the cycle guard can hold.",
		"\n}\n\n// The cycle guard", `	// SQLite's visited path is text with no declared width, so a depth that
	// MySQL refuses is one this back end runs.
	if deep, err := mdnode.Descend(ctx, ex, [][16]byte{id(0x70, 1)}, 100000); err != nil {
		t.Errorf("a deep traversal: %v", err)
	} else if len(deep) != 4 {
		t.Errorf("a deep traversal returned %d rows, want 4", len(deep))
	}
	_ = errors.Is`)
	write(t, filepath.Join(base, pkg, "live_test.go"), src)

	runGenerated(t, base+"/"+pkg, []string{
		"TestPlanLoadsEveryChildInTwoRoundTrips",
		"TestChildTopUsesTheBatchLoader",
		"TestChildQueryFiltersByItsParent",
		"TestKeysetPagingOverTheParents",
		"TestDeclaredJoinReturnsBothSidesRows",
		"TestDeclaredAggregateGroupsAndFilters",
		"TestSemiJoinDoesNotMultiplyTheParent",
		"TestUnionMergesBothTablesInOneOrder",
		"TestRowLockingInsideATransaction",
		"TestRecursiveDescendsAndAscends",
		"TestRecursiveTerminatesOnACycle",
		"TestProjectionReadsItsSubset",
		"TestUnitFlushesInForeignKeyOrder",
		"TestSoftDeleteReachesEveryCrossTableRead",
		"TestManyToManyLoadsBothDirections",
		"TestArcLoadsEveryVariantAndEnforcesExactlyOne",
		"TestAKeyIsGeneratedWhenTheCallerDoesNotSetOne",
		"TestUpdateWritesOnlyWhatWasSet",
		"TestUpdateSkipsASoftDeletedRow",
		"TestInsertAllLoadsEveryRow",
		"TestAnyOfBracketsItsConjunctions",
		"TestOffsetAndUnordered",
		"TestEveryColumnTypeRoundTripsThroughGeneratedCode",
		"TestJSONPredicates",
		"TestManyToManyThroughAPayloadModel",
		"TestSelfReferentialManyToMany",
		"TestAnyRefRoundTripsAndIsIndexable",
	})
}

func write(t *testing.T, path, src string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// litePool stands in for the caduceus pool the soft-delete fixture reads the
// table through behind storm's back: QueryRow with PostgreSQL's spelling of a
// parameter and of a uuid list, turned into SQLite's.
const litePool = `
type litePool struct{ db *sql.DB }

func (p litePool) QueryRow(ctx context.Context, q string, args ...any) *sql.Row {
	q = strings.ReplaceAll(q, " = ANY($1)", " IN (SELECT unhex(value) FROM json_each($1))")
	q = strings.ReplaceAll(q, "$", "?")
	for i, a := range args {
		switch v := a.(type) {
		case [16]byte:
			args[i] = v[:]
		case [][16]byte:
			parts := make([]string, len(v))
			for j, k := range v {
				parts[j] = "\"" + hex.EncodeToString(k[:]) + "\""
			}
			args[i] = "[" + strings.Join(parts, ",") + "]"
		}
	}
	return p.db.QueryRowContext(ctx, q, args...)
}
`

// TestGeneratedSoftDeleteRun is the soft-delete fixture PostgreSQL runs:
// deleted rows vanish from every read, a deleted row's unique value can be
// claimed again, and an upsert matches the PARTIAL unique index that makes it
// so — on SQLite, where the index is a partial UNIQUE INDEX too.
func TestGeneratedSoftDeleteRun(t *testing.T) {
	models := fixtureDecls(t, "../../codegen/softdelete_test.go", map[string]bool{"sdUser": true})
	base := "sd" + strconv.Itoa(os.Getpid())
	out := generate(t, base, models, "&sdUser{}", "sd")

	// The fixture imports the TABLE's package, one below the one generated.
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	table := ""
	for _, e := range entries {
		if e.IsDir() {
			table = e.Name()
		}
	}
	if table == "" {
		t.Fatal("no per-table package was generated")
	}

	src := fixtureConst(t, "../../codegen/softdelete_live_src_test.go", "liveTestSrc")
	src = strings.ReplaceAll(src, "PKG", table)
	src = strings.ReplaceAll(src, "IMPORTPATH",
		"github.com/gsoultan/storm/v2/internal/sqlitecheck/"+base+"/sd/"+table)
	src = replaceOnce(t, src, `"github.com/gsoultan/storm/v2/compile/pgddl"`,
		`"github.com/gsoultan/storm/v2/compile/liteddl"`)
	src = replaceOnce(t, src, `"github.com/gsoultan/storm/v2/runtime/caduceusdrv"`,
		`"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"

	"github.com/gsoultan/storm/v2/runtime/sqlitedrv"

	_ "modernc.org/sqlite"`)
	src = replaceOnce(t, src, "	pool *caduceusdrv.Pool\n", "	pool litePool\n")
	main := strings.Replace(sqliteMain, "BUILD", "&sdUser{}", 1)
	main = strings.Replace(main, "	ex = sqlitedrv.NewPool(db)\n",
		"	ex = sqlitedrv.NewPool(db)\n	pool = litePool{db}\n", 1)
	src = replaceBetween(t, src, "func TestMain(m *testing.M) {", "func must(err error) {", main+litePool+"\n")
	write(t, filepath.Join(out, table, "live_test.go"), src)

	runGenerated(t, base+"/sd/"+table, []string{
		"TestDeletedRowVanishesFromEveryRead",
		"TestDeletingTwiceReportsNoRow",
		"TestTheAddressOfADeletedRowCanBeClaimedAgain",
		"TestSeveralDeletedRowsMayShareAUniqueValue",
		"TestTwoLiveRowsStillCannotShareAUniqueValue",
		"TestRestoreBringsTheRowBack",
		"TestUpsertMatchesThePartialUniqueIndex",
		"TestUpsertDoesNotResurrectADeletedRow",
		"TestHardDeleteActuallyRemoves",
		"TestDeleteAllMarksMatchingLiveRows",
		"TestHardDeleteAllRemovesMatchingRowsMarkedOrNot",
		"TestUpdateAllSkipsDeletedRows",
		"TestOnlyAnAggregateDeclaredAcrossDeletedCountsMarkedRows",
	})
}
