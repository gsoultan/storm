package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

// A model with an ARC, generated as a context for every dialect and COMPILED.
//
// Nothing had compiled one off PostgreSQL, and doing it for Oracle found three
// defects at once:
//
//   - A nullable uuid — every optional reference, and every arc column — was
//     decoded with runtime.Nullable, which is generic over the BYTE decoders.
//     The value family has its own.
//   - The arc loader is the one read that reaches its rows through a Batch
//     callback, and it spelled RawValues out instead of asking the dialect,
//     so an Oracle context handed [][]byte to a scanner that takes []any.
//   - A context imported its decoder family unconditionally, and one whose
//     only cross-package read is an arc loader never calls it. Go rejects
//     the unused import, which is a context that does not build on every
//     family but PostgreSQL's.
//
// The model is deliberately that minimal context: an arc and nothing else
// that would call a decoder from the context file.
type arcAuthor struct {
	storm.Model
	Name string
}

func (a *arcAuthor) Schema(t *storm.Table) { t.Col(&a.Name).Size(80) }

type arcArticle struct {
	storm.Model
	Title string
}

func (a *arcArticle) Schema(t *storm.Table) { t.Col(&a.Title).Size(120) }

type arcAttachment struct {
	storm.Model
	Filename string
	Subject  storm.OneOf2[arcAuthor, arcArticle]
}

func (a *arcAttachment) Schema(t *storm.Table) { t.Col(&a.Filename).Size(120) }

func TestAContextWithAnArcCompilesOnEveryDialect(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a generated context per dialect; -short skips it")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []codegen.Dialect{
		codegen.DialectPostgres, codegen.DialectMySQL, codegen.DialectMariaDB,
		codegen.DialectMSSQL, codegen.DialectOracle,
	} {
		t.Run(d.String(), func(t *testing.T) {
			s, err := storm.Build(&arcAuthor{}, &arcArticle{}, &arcAttachment{})
			if err != nil {
				t.Fatal(err)
			}
			// Inside the module, for the reason TestPackage_Compiles gives:
			// an import path cannot be resolved from a temp dir, and the
			// point is to BUILD the result.
			rel := filepath.Join("internal", "arcctx"+strconv.Itoa(os.Getpid())+d.String())
			dir := filepath.Join(root, rel)
			t.Cleanup(func() { os.RemoveAll(dir) })

			files, err := codegen.Package(s, codegen.PackageOptions{
				Dir:           dir,
				Import:        "github.com/gsoultan/storm",
				Dialect:       d,
				Package:       "store",
				PackageImport: "github.com/gsoultan/storm/" + filepath.ToSlash(rel),
			})
			if err != nil {
				t.Fatal(err)
			}
			loader := false
			for p, src := range files {
				if strings.Contains(string(src), "ex.Batch(") {
					loader = true
				}
				full := filepath.Join(dir, p)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, src, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if !loader {
				t.Fatal("no arc loader was generated, so compiling proves nothing about it")
			}
			for _, verb := range []string{"build", "vet"} {
				cmd := exec.Command("go", verb, "./"+filepath.ToSlash(rel)+"/...")
				cmd.Dir = root
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("a %s context with an arc does not %s:\n%s", d, verb, out)
				}
			}
		})
	}
}
