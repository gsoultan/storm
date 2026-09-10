package codegen_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

// A MySQL-portable model: no arrays, no inet, no tsvector, no ranges — the
// types codegen's own supports() already refuses for this dialect.
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
}

func (m *myPortable) Schema(t *storm.Table) {
	t.Col(&m.Email).Size(320)
	t.Col(&m.Name).Size(120)
	t.Col(&m.Note).Size(400)
	t.Col(&m.Seen).Size(64)
	t.Col(&m.Balance).Numeric(18, 4)
	t.Col(&m.Amount).Numeric(18, 4)
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
	// therefore takes an explicit opt-out of the refusal an adopter gets.
	defer codegen.AllowUnexecutableMySQLForTest()()

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

// The refusal itself. A generator that emits a package no server will accept
// has produced a silent wrong answer, and that is the failure this replaces.
//
// Measured against MySQL 8.4.11, the emitted package's first read is
// Error 1064: default sql_mode has no ANSI_QUOTES, so `"my_users"` is a string
// literal rather than a table. RETURNING is Error 1064 as well, and the
// placeholders are $1 where MySQL wants ?.
func TestMySQLGenerationIsRefusedUntilTheQueryLoweringExists(t *testing.T) {
	s, err := storm.Build(&myPortable{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = codegen.Package(s, codegen.PackageOptions{
		Dir: t.TempDir(), Import: "github.com/gsoultan/storm", Dialect: codegen.DialectMySQL,
	})
	if !errors.Is(err, codegen.ErrMySQLQueryLoweringMissing) {
		t.Fatalf("generating a MySQL package returned %v; it must refuse", err)
	}
	// The refusal has to say what is missing and where to read about it —
	// an adopter who hits this needs to know it is storm's gap, not their model.
	for _, want := range []string{"compile/myddl", "compile/mysql", "runtime/mydec", "Error 1064", "M9"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
	// PostgreSQL is unaffected.
	if _, err := codegen.Package(s, codegen.PackageOptions{
		Dir: t.TempDir(), Import: "github.com/gsoultan/storm",
	}); err != nil {
		t.Fatalf("the PostgreSQL path was caught by the MySQL refusal: %v", err)
	}
}
