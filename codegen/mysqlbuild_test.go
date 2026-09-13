package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

// A MySQL-portable model: no arrays, no inet, no tsvector, no ranges — the
// types codegen's own supports() already refuses for this dialect.
// EVERY portable type, nullable and not.
//
// The fixture used to be a handful of strings and decimals, and three decoder
// mappings were missing without this gate saying so: a date assigned one value
// from a two-value function, a TIME assigned a time.Duration to a
// runtime.TimeOfDay, and JSON had no decoder at all. Each refused the package
// or produced code that would not build — which is exactly what this gate is
// for, and it passed because no column here had those types.
//
// A type added to storm belongs in this struct.
type myPortable struct {
	storm.Model
	Email   string
	Name    string
	Age     *int16
	Active  bool
	Balance storm.Decimal
	Note    *string
	Amount  *storm.Decimal
	Seen    *string

	Small   int16
	Medium  int32
	Big     int64
	Single  float32
	Double  float64
	Blob    []byte
	Stamp   time.Time
	Day     time.Time
	Clock   storm.TimeOfDay
	Doc     storm.JSON
	OptBool *bool
	OptBig  *int64
	OptDbl  *float64
	OptTime *time.Time
	OptDay  *time.Time
	OptClk  *storm.TimeOfDay
	OptBlob []byte
	OptDoc  *storm.JSON
}

func (m *myPortable) Schema(t *storm.Table) {
	t.Col(&m.Email).Size(320)
	t.Col(&m.Name).Size(120)
	t.Col(&m.Note).Size(400)
	t.Col(&m.Seen).Size(64)
	t.Col(&m.Balance).Numeric(18, 4)
	t.Col(&m.Amount).Numeric(18, 4)
	t.Col(&m.Day).Date()
	t.Col(&m.OptDay).Date()
	t.Unique(&m.Email)
	t.Index(&m.Name)
}

// R9's gate, and the one that was missing: the dialect seam had a second
// implementation whose output nothing had ever COMPILED.
//
// The existing dialect tests assert the emitted TEXT calls mydec, and passed
// while the package referenced a decoder family it did not import, called a
// generic that does not exist in that family, and assigned two return values
// to one variable. A seam with one implementation is a hypothesis; a seam
// whose second implementation does not build is a worse one, because the
// tests read as though it does.
func TestMySQLGeneratedPackageCompiles(t *testing.T) {
	// This gate asserts that the DECODE seam compiles — R9 — and nothing more.
	// The package it builds cannot execute a statement, because the query side
	// of the seam has one implementation and it is PostgreSQL's. Generating it

	s, err := storm.Build(&myPortable{})
	if err != nil {
		t.Fatal(err)
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// Inside the module: an import path cannot be resolved from a temp dir,
	// and the point is to BUILD the result.
	dir := filepath.Join(root, "internal", "mybuild"+strconv.Itoa(os.Getpid()))
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir:     dir,
		Import:  "github.com/gsoultan/storm",
		Dialect: codegen.DialectMySQL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated; the test would assert nothing")
	}
	for rel, src := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Build from the module root: `go build` resolves a ./... pattern against
	// the working directory, and the test's is codegen/.
	cmd := exec.Command("go", "build", "./internal/"+filepath.Base(dir)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("MySQL-dialect generated code does not compile:\n%s", out)
	}
}

// The gate that was missing, and the one that would have caught the original
// defect: a MySQL package must carry MySQL SQL.
//
// TestMySQLGeneratedPackageCompiles was green for four releases while the
// package it built carried PostgreSQL — double-quoted identifiers, $1
// placeholders, an output clause MySQL has not. Compiling and executing are
// different claims, and this asserts the second one's precondition.
func TestMySQLGeneratedPackageCarriesMySQLSQL(t *testing.T) {
	s, err := storm.Build(&myPortable{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: t.TempDir(), Import: "github.com/gsoultan/storm", Dialect: codegen.DialectMySQL,
	})
	if err != nil {
		t.Fatalf("generating a MySQL package: %v", err)
	}
	var all strings.Builder
	for _, b := range files {
		all.Write(b)
	}
	src := all.String()

	for _, sql := range sqlConstants(src) {
		if strings.Contains(sql, `"`) {
			t.Errorf("a double-quoted identifier reached MySQL SQL — default sql_mode "+
				"has no ANSI_QUOTES, so it is a string literal, not a name:\n  %s", sql)
		}
		if strings.Contains(sql, "$") {
			t.Errorf("a PostgreSQL placeholder reached MySQL SQL:\n  %s", sql)
		}
		if strings.Contains(strings.ToUpper(sql), "RETURNING") {
			t.Errorf("an output clause MySQL 8 does not have reached its SQL:\n  %s", sql)
		}
	}
	if !strings.Contains(src, "INSERT INTO `my_portables`") {
		t.Error("no backticked insert was emitted; the fixture or the extractor is wrong")
	}
	// And PostgreSQL is unaffected.
	pg, err := codegen.Package(s, codegen.PackageOptions{
		Dir: t.TempDir(), Import: "github.com/gsoultan/storm",
	})
	if err != nil {
		t.Fatal(err)
	}
	var pgAll strings.Builder
	for _, b := range pg {
		pgAll.Write(b)
	}
	if !strings.Contains(pgAll.String(), `FROM "my_portables"`) {
		t.Error("the PostgreSQL path stopped emitting PostgreSQL identifiers")
	}
}

// sqlConstants pulls the emitted statements back out of a generated file, in
// both literal forms — raw where the SQL has no backtick, interpreted where it
// does. Anything that reads only one form would miss exactly the dialect this
// test is about.
func sqlConstants(src string) []string {
	var out []string
	for _, m := range regexp.MustCompile("(?m)^const \\w*(?:Prefix|SQL|Suffix) = `([^`]*)`$").FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	for _, m := range regexp.MustCompile(`(?m)^const \w*(?:Prefix|SQL|Suffix) = "((?:[^"\\]|\\.)*)"$`).FindAllStringSubmatch(src, -1) {
		if s, err := strconv.Unquote(`"` + m[1] + `"`); err == nil {
			out = append(out, s)
		}
	}
	return out
}
