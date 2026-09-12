package codegen_test

import (
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/compile/myddl"
	"github.com/gsoultan/storm/schema"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The end-to-end that has never happened: a GENERATED storm package executing
// against a real MySQL-family server, through storm's own adapter.
//
// Everything before this proved a piece — the SQL PREPAREs, mydec decodes the
// bytes, the adapter satisfies the port. None of them proved the pieces fit,
// and "each part works" is the claim that has been wrong twice in M9 already.
func TestGeneratedMariaDBPackageRunsAgainstAServer(t *testing.T) {
	if os.Getenv("STORM_MYSQL_ADDR") == "" {
		t.Skip("STORM_MYSQL_ADDR unset")
	}
	// NOT buildSoftDelete's model: its t.Unique is scoped to the live rows,
	// which is a PARTIAL unique index, and MySQL has none — myddl.Check
	// refuses it, correctly. On this engine a soft-delete table can have
	// uniqueness across ALL rows or none, and that is an engine limit rather
	// than a storm gap. See TestSoftDeleteScopedUniqueDoesNotPort.
	s, err := storm.Build(&mdUser{})
	if err != nil {
		t.Fatal(err)
	}
	root, err2 := filepath.Abs("..")
	if err2 != nil {
		t.Fatal(err2)
	}
	base := "mdlive" + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := pkgFor(t, s, dir, "mariadb")
	if err != nil {
		t.Fatalf("generating for MariaDB: %v", err)
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
	src := strings.ReplaceAll(mariadbLiveSrc, "PKG", pkg)
	src = strings.ReplaceAll(src, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base+"/"+pkg)
	if err := os.WriteFile(filepath.Join(pkgDir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/"+pkg+"/")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "STORM_MYSQL_ADDR="+os.Getenv("STORM_MYSQL_ADDR"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the generated package does not run:\n%s", out)
	}
	// A subprocess that skipped is a green test that proved nothing — the trap
	// the soft-delete live test already fell into once.
	for _, name := range []string{"TestInsertSelectUpdateDelete", "TestInsertReturnsTheRow"} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

func pkgFor(t *testing.T, s *schema.Schema, dir, dialect string) (map[string][]byte, error) {
	t.Helper()
	d := codegen.DialectPostgres
	switch dialect {
	case "mysql":
		d = codegen.DialectMySQL
	case "mariadb":
		d = codegen.DialectMariaDB
	}
	return codegen.Package(s, codegen.PackageOptions{
		Dir: dir, Import: "github.com/gsoultan/storm", Dialect: d,
	})
}

// mdUser is a soft-delete model that PORTS: the email is sized so it is a
// VARCHAR rather than LONGTEXT, and its uniqueness spans deleted rows, because
// the live-scoped form is a partial index MySQL cannot express.
type mdUser struct {
	storm.Model
	Email     string
	Name      string
	DeletedAt *time.Time
}

func (u *mdUser) Schema(t *storm.Table) {
	t.SoftDelete(&u.DeletedAt)
	t.Col(&u.Email).Size(320)
	t.Col(&u.Name).Size(120)
	t.UniqueAcrossDeleted(&u.Email)
}

// The limit, stated as a test so it is not rediscovered. storm scopes a
// soft-delete table's uniqueness to the live rows with a PARTIAL unique index,
// which is the only way PostgreSQL can say "unique among the rows that are
// alive" — and MySQL has no partial index at all. myddl.Check refuses it, and
// the message says the rows it excludes would be indexed too.
//
// So on this engine a soft-delete table has uniqueness over EVERY row or none.
// That is a real behavioural difference for an adopter: a deleted row keeps its
// email forever, where on PostgreSQL the address becomes claimable again.
func TestSoftDeleteScopedUniqueDoesNotPortToMySQL(t *testing.T) {
	s := buildSoftDelete(t)
	err := myddl.Check(s)
	if err == nil {
		t.Fatal("a live-scoped unique index ported to MySQL; it has no partial index")
	}
	if !strings.Contains(err.Error(), "partial") {
		t.Errorf("the refusal does not name the reason:\n%v", err)
	}
}

// A sixth MariaDB divergence, found by the end-to-end and recorded rather than
// fixed here — fixing it means splitting compile/myddl per dialect, which is
// its own change.
//
// MySQL 8.4 accepts a generated column declared `... STORED NOT NULL`.
// MariaDB 11.4 rejects it: its grammar allows no nullability clause after
// VIRTUAL/PERSISTENT/STORED. So a model with a NOT NULL generated column emits
// DDL MariaDB will not apply.
func TestGeneratedColumnDDLDoesNotPortToMariaDB(t *testing.T) {
	t.Skip("known gap: compile/myddl emits `STORED NOT NULL`, which MariaDB rejects — " +
		"needs a MariaDB DDL variant, see docs/PLAN.md M9")
}
