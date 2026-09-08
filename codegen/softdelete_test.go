package codegen_test

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
	"github.com/gsoultan/storm/schema"
)

// Soft delete is opt-in per table (docs/CONCEPT.md), and the reason it is
// offered here at all is that a compiler can make the predicate impossible to
// forget where a runtime ORM can only ask you to remember. These tests are
// about that claim: not that the predicate CAN be emitted, but that there is no
// read of the table which does not carry it.

type sdUser struct {
	storm.Model
	Email     string
	Name      string
	DeletedAt *time.Time
}

func (u *sdUser) Schema(t *storm.Table) {
	t.SoftDelete(&u.DeletedAt)
	t.Unique(&u.Email)
}

func buildSoftDelete(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := storm.Build(&sdUser{})
	if err != nil {
		t.Fatalf("a valid soft-delete model did not build: %v", err)
	}
	return s
}

func genSoftDelete(t *testing.T) string {
	t.Helper()
	s := buildSoftDelete(t)
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: "store", Import: "github.com/gsoultan/storm",
	})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, src := range files {
		b.Write(src)
	}
	if b.Len() == 0 {
		t.Fatal("no files generated; the test would assert nothing")
	}
	return b.String()
}

// The load-bearing one. Every statement that reads the table has to carry the
// predicate — and the way to test "every" is to find the reads and check them
// all, not to check the three somebody remembered to list.
func TestEveryReadOfASoftDeleteTableIsGuarded(t *testing.T) {
	src := genSoftDelete(t)

	if !strings.Contains(src, "const softDeleteWhere = ") {
		t.Fatal("no softDeleteWhere constant was emitted")
	}
	// runtime.SpliceTree is the UNGUARDED splice. On a soft-delete table every
	// read must use SpliceTreeWhere instead, so any bare SpliceTree( here is a
	// read that would return deleted rows.
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "runtime.SpliceTree(") {
			t.Errorf("line %d reads without the soft-delete predicate:\n  %s", i+1, strings.TrimSpace(line))
		}
	}
	for _, want := range []string{
		"runtime.SpliceTreeWhere(selectPrefix, softDeleteWhere,",
		"runtime.SpliceTreeWhere(existsPrefix, softDeleteWhere,",
		"runtime.SpliceTreeWhere(countPrefix, softDeleteWhere,",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing guarded read: %s", want)
		}
	}
}

// Delete must mark; the row has to survive. And the batch form has to make the
// SAME choice — a queued DeleteOp that hard-deletes would destroy rows the
// caller believed were recoverable, and nothing at the call site would say so.
func TestSoftDeleteWriteFunctions(t *testing.T) {
	src := genSoftDelete(t)

	for _, want := range []string{
		"func Delete(",
		"func HardDelete(",
		"func Restore(",
		"func DeleteOp(",
		"func HardDeleteOp(",
		"func RestoreOp(",
		`UPDATE "sd_users" SET "deleted_at" = now()`,
		`UPDATE "sd_users" SET "deleted_at" = NULL`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing from a soft-delete package: %s", want)
		}
	}
	if !strings.Contains(src, "runtime.BatchOp{SQL: softDeleteSQL") {
		t.Error("DeleteOp does not mark the row — a queued delete would destroy it")
	}
	if strings.Contains(src, "runtime.BatchOp{SQL: deleteSQL") {
		t.Error("DeleteOp still points at the hard delete")
	}
}

// An UPDATE that finds a deleted row would write through a row every read says
// is gone.
func TestUpdateSkipsDeletedRows(t *testing.T) {
	src := genSoftDelete(t)
	if !strings.Contains(src, "// A deleted row is not updatable") {
		t.Error("the update path does not exclude deleted rows")
	}
}

// A table that does NOT soft-delete must be byte-for-byte unaffected: this
// feature is opt-in, and an opt-in feature that changes the output of models
// that did not opt in is not opt-in.
//
// The first version of this test asserted on SHAPE — that the ordinary splice
// was still there and the soft-delete machinery was not — and passed while the
// feature was in fact changing every generated file in the repository: a
// capacity hint had been widened unconditionally, from `0, 2` to `0, 3`. Wrong
// by nothing at run time, and still a diff in every adopter's tree on upgrade.
// The in-tree fixtures under bench/ and internal/planspike/ are the real
// guard — scripts/check/boundaries.sh regenerates and fails on any drift — and
// this test now checks the specific thing that slipped past.
func TestNonSoftDeleteOutputIsUnchanged(t *testing.T) {
	src := genSoftDelete(t)
	if !strings.Contains(src, "softDeleteWhere") {
		t.Fatal("fixture is wrong: the soft-delete package has no predicate")
	}
	type plain struct {
		storm.Model
		Email string
	}
	s, err := storm.Build(&plain{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: "store", Import: "github.com/gsoultan/storm",
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range files {
		body := string(b)
		if strings.Contains(body, "softDeleteWhere") || strings.Contains(body, "HardDelete") {
			t.Errorf("%s: a table that does not soft-delete carries soft-delete machinery", name)
		}
		if !strings.Contains(body, "runtime.SpliceTree(selectPrefix,") {
			t.Errorf("%s: the ordinary read path changed shape", name)
		}
		// The capacity that was widened for everyone. A table with a single-
		// column key and no version column appends the key and nothing else.
		if !strings.Contains(body, "where := make([]runtime.Frag, 0, 2)") {
			t.Errorf("%s: the update scratch buffer is sized for a soft delete "+
				"this table does not have", name)
		}
	}
}

// And the whole thing has to compile. The staleness assertion and the shape
// assertion both live in generated code, so "it produced plausible text" is not
// the same claim as "it produced a package".
func TestSoftDeleteGeneratedPackageCompiles(t *testing.T) {
	s := buildSoftDelete(t)
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "internal", "sdbuild"+strconv.Itoa(os.Getpid()))
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: dir, Import: "github.com/gsoultan/storm",
	})
	if err != nil {
		t.Fatal(err)
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
	cmd := exec.Command("go", "build", "./internal/"+filepath.Base(dir)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("a soft-delete package does not compile:\n%s", out)
	}
}

// The behaviour, against a real database. Everything above asserts on generated
// TEXT, which is one plausible-looking step away from asserting nothing: the
// question that matters is whether a deleted row actually stops coming back,
// and whether the address it was using can be claimed again.
//
// The package under test does not exist until this test writes it, so the test
// generates it into the module, writes a test file beside it, and runs `go
// test` on the result.
func TestSoftDeleteBehaviourAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s := buildSoftDelete(t)
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "sdlive" + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: dir, Import: "github.com/gsoultan/storm",
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
	src := strings.ReplaceAll(liveTestSrc, "PKG", pkg)
	src = strings.ReplaceAll(src, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base+"/"+pkg)
	if err := os.WriteFile(filepath.Join(pkgDir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/"+pkg+"/")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "STORM_DSN="+os.Getenv("STORM_DSN"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("soft delete does not behave against a real database:\n%s", out)
	}
	// A green subprocess that ran nothing is the failure mode this guards: if
	// the environment did not survive the exec, TestMain would once have
	// skipped and reported success for an untested feature.
	for _, name := range []string{
		"TestDeletedRowVanishesFromEveryRead",
		"TestDeletingTwiceReportsNoRow",
		"TestTheAddressOfADeletedRowCanBeClaimedAgain",
		"TestSeveralDeletedRowsMayShareAUniqueValue",
		"TestTwoLiveRowsStillCannotShareAUniqueValue",
		"TestRestoreBringsTheRowBack",
		"TestUpsertMatchesThePartialUniqueIndex",
		"TestUpsertDoesNotResurrectADeletedRow",
		"TestHardDeleteActuallyRemoves",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}
