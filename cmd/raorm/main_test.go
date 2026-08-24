package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/raorm/internal/testmodel"
)

// The CLI is the surface every user touches first, and a tool that fails
// unhelpfully is a tool people work around. These assert the MESSAGES, not just
// the exit status: "no models registered" pointing at the docs is the
// difference between a five-minute start and an afternoon.

func withModels(t *testing.T, m []any) {
	t.Helper()
	prev := Models
	Models = m
	t.Cleanup(func() { Models = prev })
}

func TestCLI_NoCommand(t *testing.T) {
	withModels(t, testmodel.All())
	err := run(nil)
	if err == nil {
		t.Fatal("running with no command must fail")
	}
	if !strings.Contains(err.Error(), "no command") {
		t.Errorf("error should say what is missing, got: %v", err)
	}
}

func TestCLI_UnknownCommand(t *testing.T) {
	withModels(t, testmodel.All())
	err := run([]string{"frobnicate"})
	if err == nil {
		t.Fatal("an unknown command must fail")
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("the error should name the command, got: %v", err)
	}
}

// The shipped binary is a template: it has no models until a user's module
// provides them, and it has to say so in a way that leads somewhere.
func TestCLI_NoModelsRegistered(t *testing.T) {
	withModels(t, nil)
	err := run([]string{"ddl"})
	if err == nil {
		t.Fatal("a binary with no models must fail rather than print nothing")
	}
	for _, want := range []string{"no models registered", "EXAMPLE.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q, got: %v", want, err)
		}
	}
}

func TestCLI_DDL(t *testing.T) {
	withModels(t, testmodel.All())
	out := captureStdout(t, func() {
		if err := run([]string{"ddl"}); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{`CREATE TABLE "users"`, `CREATE TABLE "orgs"`, "PRIMARY KEY"} {
		if !strings.Contains(out, want) {
			t.Errorf("ddl output is missing %q", want)
		}
	}
}

func TestCLI_GenerateThenVerifyStale(t *testing.T) {
	withModels(t, testmodel.All())
	dir := filepath.Join(t.TempDir(), "store")

	if err := run([]string{"generate", dir}); err != nil {
		t.Fatal(err)
	}
	// Freshly generated code must verify.
	if err := run([]string{"verify", "-stale", dir}); err != nil {
		t.Fatalf("code that was just generated is reported stale: %v", err)
	}

	// Regenerating changes nothing: the output is deterministic, which is what
	// makes a stale check meaningful at all.
	before := treeHash(t, dir)
	if err := run([]string{"generate", dir}); err != nil {
		t.Fatal(err)
	}
	if after := treeHash(t, dir); after != before {
		t.Error("regenerating changed the tree — the stale check would fail at random")
	}
}

// The check that belongs in CI: a hand-edit must be caught, and the error must
// say how to fix it.
func TestCLI_VerifyStaleDetectsAnEdit(t *testing.T) {
	withModels(t, testmodel.All())
	dir := filepath.Join(t.TempDir(), "store")
	if err := run([]string{"generate", dir}); err != nil {
		t.Fatal(err)
	}

	victim := filepath.Join(dir, "user", "user.gen.go")
	src, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, append(src, []byte("\n// hand-edited\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	err = run([]string{"verify", "-stale", dir})
	if err == nil {
		t.Fatal("a hand-edited generated file must be reported stale")
	}
	if !strings.Contains(err.Error(), "raorm generate") {
		t.Errorf("the error should say how to fix it, got: %v", err)
	}
}

// A missing file is as stale as a wrong one, and must not be silently ignored.
func TestCLI_VerifyStaleDetectsADeletion(t *testing.T) {
	withModels(t, testmodel.All())
	dir := filepath.Join(t.TempDir(), "store")
	if err := run([]string{"generate", dir}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "user")); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", "-stale", dir}); err == nil {
		t.Fatal("a deleted package must be reported stale")
	}
}

// Commands needing a database must say so rather than failing obscurely.
func TestCLI_DatabaseCommandsNeedADSN(t *testing.T) {
	withModels(t, testmodel.All())
	t.Setenv("RAORM_DSN", "")
	for _, cmd := range [][]string{{"verify"}, {"diff", "x"}, {"import"}} {
		err := run(cmd)
		if err == nil {
			t.Errorf("%v without a DSN must fail", cmd)
			continue
		}
		if !strings.Contains(err.Error(), "dsn") && !strings.Contains(err.Error(), "DSN") {
			t.Errorf("%v: the error should name the missing DSN, got: %v", cmd, err)
		}
	}
}

func TestCLI_DiffNeedsAName(t *testing.T) {
	withModels(t, testmodel.All())
	err := run([]string{"diff"})
	if err == nil {
		t.Fatal("diff without a name must fail")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("the error should say a name is needed, got: %v", err)
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	f()
	w.Close()
	os.Stdout = prev
	return <-done
}

func treeHash(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		b.WriteString(p)
		b.Write(src)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// lint costs every named plan and fails over the budget — plans.go can be
// BUDGETED, and a plan that quietly grew fails CI naming itself instead of
// failing a latency SLO naming nothing.
func TestCLI_LintPlans(t *testing.T) {
	withModels(t, testmodel.All())

	out := captureStdout(t, func() {
		if err := run([]string{"lint"}); err != nil {
			t.Fatalf("the fixture's plans all fit the default budget of 4: %v", err)
		}
	})
	for _, want := range []string{"UserFeed", "4 round trip", "UserSummary", "OrgTree", "= ANY"} {
		if !strings.Contains(out, want) {
			t.Errorf("lint output is missing %q:\n%s", want, out)
		}
	}

	// Tighten the budget below UserFeed's cost, and it must fail naming a fix.
	err := run([]string{"lint", "-max-round-trips", "3"})
	if err == nil {
		t.Fatal("UserFeed costs 4; a budget of 3 must fail")
	}
	for _, want := range []string{"exceed", "split the plan"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q, got: %v", want, err)
		}
	}
}
