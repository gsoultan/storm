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

	"github.com/gsoultan/storm/v2"
	"github.com/gsoultan/storm/v2/codegen"
)

// Full-text search on the MySQL family.
//
// A FULLTEXT index could be declared and migrated for a year with nothing in
// the generated API able to ask it a question: every search went through raw
// SQL. Matches is now offered on a text column that a FULLTEXT index covers on
// its OWN, because InnoDB answers MATCH only through an index over exactly its
// column list (error 1191 otherwise) — and on no other column, which is why
// such a column gets its own handle type rather than a method on TextCol.
type ftArticle struct {
	storm.Model
	Title string
	Body  string
	Slug  string
	Note  *string
}

func (m *ftArticle) Schema(t *storm.Table) {
	t.Col(&m.Title).Size(200)
	t.Col(&m.Slug).Size(200)
	t.Index(&m.Body).Using(storm.FullText).Named("ix_ft_body")
	t.Index(&m.Note).Using(storm.FullText).Named("ix_ft_note")
	// Two columns, one index: only MATCH (title, slug) could use it, which a
	// per-column handle cannot spell. Neither column may offer Matches.
	t.Index(&m.Title, &m.Slug).Using(storm.FullText).Named("ix_ft_title_slug")
}

func ftSource(t *testing.T, d codegen.Dialect) string {
	t.Helper()
	s, err := storm.Build(&ftArticle{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: t.TempDir(), Import: "github.com/gsoultan/storm/v2", Dialect: d,
	})
	if err != nil {
		t.Fatalf("generating for %s: %v", d, err)
	}
	var all strings.Builder
	for _, b := range files {
		all.Write(b)
	}
	return all.String()
}

func TestFullTextIndexedColumnOffersMatches(t *testing.T) {
	for _, d := range []codegen.Dialect{codegen.DialectMySQL, codegen.DialectMariaDB} {
		src := ftSource(t, d)
		for _, want := range []string{
			"Body      = FullTextCol{",
			"Note      = NullFullTextCol{",
			"func (h FullTextCol) Matches(v string) Pred",
			"func (h NullFullTextCol) Matches(v string) Pred",
			"func (q Query) BodyMatches(v string) Query",
			// Still a text column: every text predicate stays.
			"func (h FullTextCol) Like(v string) Pred",
			"func (h FullTextCol) Eq(v string) Pred",
			`runtime.OpMatches:       {A: "MATCH (` + "`body`" + `", B: ") AGAINST (REPLACE(?, '*', ' ') IN NATURAL LANGUAGE MODE)"}`,
		} {
			if !strings.Contains(src, want) {
				t.Errorf("%s: missing %q", d, want)
			}
		}
		for _, banned := range []string{
			"func (h TextCol) Matches",
			"TitleMatches",
			"SlugMatches",
			// No second meaning for the PostgreSQL method: see compile/mysql.
			"FullTextCol) WebSearch",
			"BOOLEAN MODE",
		} {
			if strings.Contains(src, banned) {
				t.Errorf("%s: %q was emitted", d, banned)
			}
		}
	}
}

// PostgreSQL's answer is a tsvector column, not a FULLTEXT index: a text
// column there never gets Matches, whatever indexes the model declares.
func TestFullTextHandleIsMySQLFamilyOnly(t *testing.T) {
	for _, d := range []codegen.Dialect{codegen.DialectPostgres, codegen.DialectMSSQL, codegen.DialectOracle} {
		src := ftSource(t, d)
		if strings.Contains(src, "FullTextCol") || strings.Contains(src, "MATCH (") {
			t.Errorf("%s: a FULLTEXT handle was generated for a target without FULLTEXT", d)
		}
	}
}

// Text assertions prove the generator agrees with itself; the package has to
// BUILD, with the new handle type, its methods and its chained sugar.
func TestFullTextPackageCompiles(t *testing.T) {
	s, err := storm.Build(&ftArticle{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "internal", "ftbuild"+strconv.Itoa(os.Getpid()))
	t.Cleanup(func() { os.RemoveAll(dir) })
	files, err := pkgFor(t, s, dir, "mariadb")
	if err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, files)
	cmd := exec.Command("go", "vet", "./internal/"+filepath.Base(dir)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the generated package does not build:\n%s", out)
	}
}

// The claim that matters, against a real server: the statement PREPAREs, the
// term is bound, and the index answers. Run for each engine whose address is
// set, with the dialect generated for it.
func TestFullTextSearchRunsAgainstMariaDB(t *testing.T) {
	runFullTextLive(t, "mariadb", "STORM_MARIADB_ADDR")
}

func TestFullTextSearchRunsAgainstMySQL(t *testing.T) {
	runFullTextLive(t, "mysql", "STORM_MYSQL_ADDR")
}

func runFullTextLive(t *testing.T, dialect, addrVar string) {
	if os.Getenv(addrVar) == "" {
		t.Skip(addrVar + " unset")
	}
	sweepGenerated(t, "ftlive")
	s, err := storm.Build(&ftArticle{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "ftlive" + dialect + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })
	files, err := pkgFor(t, s, dir, dialect)
	if err != nil {
		t.Fatalf("generating for %s: %v", dialect, err)
	}
	pkgDir := writeFiles(t, dir, files)
	pkg := filepath.Base(pkgDir)
	src := strings.ReplaceAll(fullTextLiveSrc, "PKG", pkg)
	src = strings.ReplaceAll(src, "ADDRVAR", addrVar)
	src = strings.ReplaceAll(src, "IMPORTPATH", "github.com/gsoultan/storm/v2/internal/"+base+"/"+pkg)
	if err := os.WriteFile(filepath.Join(pkgDir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-count=1", "-v", "-timeout", "120s", "./internal/"+base+"/"+pkg+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated package does not run:\n%s", out)
	}
	// A subprocess that skipped is a green test that proved nothing.
	if !strings.Contains(string(out), "--- PASS: TestMatches") {
		t.Errorf("TestMatches did not run:\n%s", out)
	}
}

// writeFiles writes a generated package to disk and returns the directory of
// its per-table package.
func writeFiles(t *testing.T, dir string, files map[string][]byte) string {
	t.Helper()
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
		t.Fatal("no per-table package generated")
	}
	return pkgDir
}

// fullTextLiveSrc runs inside the generated package.
//
// InnoDB's natural-language relevance is TF·IDF², so a word present in EVERY
// row has an IDF of zero and matches nothing. The fixture's words are each in
// some rows and not others, which is the case a search box is for.
const fullTextLiveSrc = `package PKG_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gsoultan/storm/v2"
	"github.com/gsoultan/storm/v2/compile/myddl"
	"github.com/gsoultan/storm/v2/runtime"
	"github.com/gsoultan/storm/v2/runtime/mydrv"
	ft "IMPORTPATH"
)

type ftArticle struct {
	storm.Model
	Title string
	Body  string
	Slug  string
	Note  *string
}

func (m *ftArticle) Schema(t *storm.Table) {
	t.Col(&m.Title).Size(200)
	t.Col(&m.Slug).Size(200)
	t.Index(&m.Body).Using(storm.FullText).Named("ix_ft_body")
	t.Index(&m.Note).Using(storm.FullText).Named("ix_ft_note")
	t.Index(&m.Title, &m.Slug).Using(storm.FullText).Named("ix_ft_title_slug")
}

func TestMatches(t *testing.T) {
	ctx := context.Background()
	c, err := mydrv.NewPool(ctx, mydrv.Config{
		Addr: os.Getenv("ADDRVAR"), User: "root", Password: "storm", Database: "storm",
		AllowCleartextPasswordOverPlaintext: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s, err := storm.Build(&ftArticle{})
	if err != nil {
		t.Fatal(err)
	}
	ddl, err := myddl.Create(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "DROP TABLE IF EXISTS ` + "`ft_articles`" + `", nil); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range strings.Split(ddl, ";") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			if _, err := c.Exec(ctx, stmt, nil); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}

	for i, body := range []string{
		"storm compiles every query at build time",
		"postgres answers full text search with a tsvector",
		"mysql answers it with a fulltext index and match against",
		"sqlite is not a target",
	} {
		r := &ft.Row{Title: "t", Body: body, Slug: "s"}
		r.ID[0], r.ID[15] = byte(i+1), byte(i+1)
		if i == 0 {
			r.Note = runtime.Null[string]{V: "a note about indexes", Valid: true}
		}
		if err := ft.Insert(ctx, c, r); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	bodies := func(q ft.Query) []string {
		t.Helper()
		rows, err := q.All(ctx, c, nil)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.Body)
		}
		slices.Sort(out)
		return out
	}

	if got := bodies(ft.New().Where(ft.Body.Matches("tsvector"))); len(got) != 1 || !strings.Contains(got[0], "tsvector") {
		t.Errorf("Matches(tsvector) = %q", got)
	}
	// Natural language: either word is enough, as a search box expects.
	if got := bodies(ft.New().BodyMatches("postgres mysql")); len(got) != 2 {
		t.Errorf("Matches(postgres mysql) = %q, want both engines", got)
	}
	// Composes with every other predicate, and negates.
	if got := bodies(ft.New().Where(ft.Body.Matches("answers"), ft.Body.Like("%mysql%"))); len(got) != 1 {
		t.Errorf("Matches AND Like = %q", got)
	}
	if got := bodies(ft.New().Not(ft.Body.Matches("answers"))); len(got) != 2 {
		t.Errorf("NOT Matches = %q", got)
	}
	// A nullable column, and its own single-column index.
	if got := bodies(ft.New().Where(ft.Note.Matches("indexes"))); len(got) != 1 {
		t.Errorf("Note.Matches = %q", got)
	}
	// The term is BOUND, and boolean-mode operators are not operators:
	// "+postgres -tsvector" is the two words, so it finds the row -tsvector
	// would have excluded.
	if got := bodies(ft.New().BodyMatches("+postgres -tsvector")); len(got) != 1 || !strings.Contains(got[0], "postgres") {
		t.Errorf("Matches(+postgres -tsvector) = %q", got)
	}
	// Each of these is a search for nothing — and must not be an error. A
	// lone * was error 1064 on both engines until the lowering replaced it.
	for _, hostile := range []string{"'); DROP TABLE ft_articles; --", "\"unterminated", "*", " * ", "\"*", "*%", "", "-", "@", "~<>()"} {
		if got := bodies(ft.New().Where(ft.Body.Matches(hostile))); len(got) != 0 {
			t.Errorf("Matches(%q) = %q, want nothing", hostile, got)
		}
	}
	// And a * beside a word is a delimiter, as it was before the REPLACE.
	if got := bodies(ft.New().BodyMatches("tsvector*")); len(got) != 1 {
		t.Errorf("Matches(tsvector*) = %q", got)
	}
	if n, err := ft.New().Count(ctx, c); err != nil || n != 4 {
		t.Fatalf("the table survived the hostile terms? count=%d err=%v", n, err)
	}
}
`
