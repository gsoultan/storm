package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gsoultan/raorm/codegen"
)

// Parsing is not the bar. A generated package that parses and does not compile
// fails in the adopter's build, which is the one place a codegen bug is most
// expensive to diagnose — so the whole fixture context is generated and built.
//
// It builds inside this module rather than a scratch one so the test needs no
// network and no go.sum surgery.
func TestPackage_Compiles(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the fixture context; -short skips it")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("internal", "gentest"+strconv.Itoa(os.Getpid()))
	dir := filepath.Join(root, rel)
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := codegen.Package(fixtureSchema(t), codegen.PackageOptions{
		Dir:    dir,
		Import: "github.com/gsoultan/raorm",
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, src := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("go", "build", "./"+filepath.ToSlash(rel)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated context does not compile: %v\n%s", err, out)
	}

	// go vet catches what the compiler allows but nobody wants to review:
	// shadowed returns, unreachable code, printf mismatches in error text.
	cmd = exec.Command("go", "vet", "./"+filepath.ToSlash(rel)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated context does not vet clean: %v\n%s", err, out)
	}
}
