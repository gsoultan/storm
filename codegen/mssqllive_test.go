package codegen_test

// The generated SQL Server package, RUN.
//
// docs/PRODUCTION-READINESS.md P6.7 is the reason this exists and the reason it
// executes rather than asserting on text: twelve defects came out of running
// the MySQL lowering for the first time, and every one of them was invisible to
// a generator that agreed with itself. The same gate, one dialect later — and
// this time with a driver storm wrote, so it covers the wire as well as the SQL.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/msddl"
)

func TestGeneratedMSSQLPackageRunsAgainstAServer(t *testing.T) {
	addrVar := "STORM_MSSQL_ADDR"
	if os.Getenv(addrVar) == "" {
		t.Skip(addrVar + " unset")
	}
	sweepGenerated(t, "mslive")

	// The soft-delete model with a LIVE-SCOPED unique, which is the one MySQL
	// cannot take: it is a partial index, and myddl.Check refuses it. SQL
	// Server has filtered indexes, so the model that does not port there does
	// port here — and running it is how that claim stops being a claim.
	s, err := storm.Build(&msUser{})
	if err != nil {
		t.Fatal(err)
	}
	ddl, err := msddl.Create(s)
	if err != nil {
		t.Fatalf("msddl refused a model it should accept: %v", err)
	}
	if !strings.Contains(ddl, "WHERE deleted_at IS NULL") {
		t.Errorf("the live-scoped unique lost its filter, so a deleted row keeps "+
			"its email reserved forever:\n%s", ddl)
	}

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "mslive" + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := pkgFor(t, s, dir, "mssql")
	if err != nil {
		t.Fatalf("generating for mssql: %v", err)
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
		t.Fatal("no per-table package generated")
	}
	pkg := filepath.Base(pkgDir)

	src := strings.ReplaceAll(mssqlLiveSrc, "PKG", pkg)
	src = strings.ReplaceAll(src, "IMPORTPATH",
		"github.com/gsoultan/storm/internal/"+base+"/"+pkg)
	if err := os.WriteFile(filepath.Join(pkgDir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "-timeout", "120s",
		"./internal/"+base+"/"+pkg+"/")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		addrVar+"="+os.Getenv(addrVar),
		"STORM_MSSQL_PASSWORD="+os.Getenv("STORM_MSSQL_PASSWORD"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated package does not run:\n%s", out)
	}
	// A subprocess that SKIPPED is a green test that proved nothing.
	for _, name := range []string{
		"TestInsertSelectUpdateDelete", "TestInsertReturnsTheRow",
		"TestSoftDeleteScopedUnique", "TestPagingAndKeyset", "TestLocking",
		"TestUpsertIsAMerge",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

// msUser is the soft-delete model MySQL cannot take.
type msUser struct {
	storm.Model
	Email     string
	Name      string
	Rank      int64
	DeletedAt *time.Time
}

func (u *msUser) Schema(t *storm.Table) {
	t.Col(&u.Email).Size(255)
	t.Col(&u.Name).Size(120)
	t.SoftDelete(&u.DeletedAt)
	// LIVE-SCOPED: a deleted row must not keep its email reserved. This is the
	// partial unique index myddl.Check refuses and msddl emits.
	t.Unique(&u.Email)
}
