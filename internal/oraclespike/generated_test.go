package orabench

// The whole stack, RUN: model → generator → generated package → runtime/sqldrv
// → go-ora → Oracle.
//
// docs/PRODUCTION-READINESS.md P6.7, one level up from the lowering gate. That
// one proves storm's SQL runs; this proves the CODE storm writes runs, which is
// a different claim and the one an adopter actually cares about. M9's lesson
// was that a generated package which COMPILES is not a generated package that
// WORKS — and this milestone added a second row shape to the port, so the whole
// scan path here is one no other target exercises.
//
// It generates into THIS module, not into storm's, because running needs go-ora
// and storm's go.mod does not have it — the same reason this module exists.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

type genUser struct {
	storm.Model
	Email     string
	Name      string
	Rank      int64
	Balance   storm.Decimal
	Active    bool
	DeletedAt *time.Time
}

func (u *genUser) Schema(t *storm.Table) {
	t.Col(&u.Email).Size(255)
	t.Col(&u.Name).Size(120)
	t.Col(&u.Balance).Numeric(19, 4)
	t.SoftDelete(&u.DeletedAt)
	t.Index(&u.Email).Unique().Where(`"deleted_at" IS NULL`)
}

func TestTheGeneratedOraclePackageRuns(t *testing.T) {
	if os.Getenv(dsnEnv) == "" {
		t.Skip(dsnEnv + " unset")
	}
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	base := "oralive" + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, base)
	t.Cleanup(func() { os.RemoveAll(dir) })

	s, err := storm.Build(&genUser{})
	if err != nil {
		t.Fatal(err)
	}
	const pkg = "orauser"
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir:           filepath.Join(dir, pkg),
		Import:        "github.com/gsoultan/storm",
		Package:       pkg,
		PackageImport: "github.com/gsoultan/storm/internal/oraclespike/" + base + "/" + pkg,
		Dialect:       codegen.DialectOracle,
	})
	if err != nil {
		t.Fatalf("generating an Oracle package: %v", err)
	}
	for rel, b := range files {
		full := filepath.Join(dir, pkg, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The package holding the generated table lives one level down; find it,
	// because codegen names the directory after the table.
	tableDir, importPath := findTablePkg(t, dir, pkg, base)
	src := strings.ReplaceAll(generatedLiveSrc, "PKG", filepath.Base(tableDir))
	src = strings.ReplaceAll(src, "IMPORTPATH", importPath)
	if err := os.WriteFile(filepath.Join(tableDir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "-timeout", "180s",
		"./"+base+"/"+filepath.Base(filepath.Dir(tableDir))+"/"+filepath.Base(tableDir)+"/")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), dsnEnv+"="+os.Getenv(dsnEnv))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated package does not run:\n%s", out)
	}
	// A subprocess that SKIPPED is a green test that proved nothing — the same
	// discipline scripts/check/mssql.sh applies by counting statements.
	if !strings.Contains(string(out), "--- PASS: TestInsertSelectUpdateDelete") {
		t.Fatalf("the generated package's own tests did not run:\n%s", out)
	}
	t.Logf("%s", out)
}

// findTablePkg locates the directory codegen put the table package in.
func findTablePkg(t *testing.T, dir, pkg, base string) (string, string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, pkg))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(dir, pkg, e.Name()),
				"github.com/gsoultan/storm/internal/oraclespike/" + base + "/" + pkg + "/" + e.Name()
		}
	}
	t.Fatalf("codegen produced no table package under %s", filepath.Join(dir, pkg))
	return "", ""
}
