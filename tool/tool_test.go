package tool

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/internal/testmodel"
)

// The CLI is the surface every user touches first, and a tool that fails
// unhelpfully is a tool people work around. These assert the MESSAGES, not just
// the exit status: "no models registered" pointing at the docs is the
// difference between a five-minute start and an afternoon.

// moduleScratch is a per-test output directory INSIDE the module. generate
// derives import paths from the module root, so t.TempDir() — outside it — is
// now rejected; the old tests used it anyway, and their generated files
// carried broken import paths that nothing noticed because nothing built them.
func moduleScratch(t *testing.T, name string) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(root, "internal", name+strconv.Itoa(os.Getpid()))
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

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
	dir := filepath.Join(moduleScratch(t, "clistale"), "store")

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
	dir := filepath.Join(moduleScratch(t, "clistale"), "store")
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
	if !strings.Contains(err.Error(), "storm generate") {
		t.Errorf("the error should say how to fix it, got: %v", err)
	}
}

// A missing file is as stale as a wrong one, and must not be silently ignored.
func TestCLI_VerifyStaleDetectsADeletion(t *testing.T) {
	withModels(t, testmodel.All())
	dir := filepath.Join(moduleScratch(t, "clistale"), "store")
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
	t.Setenv("STORM_DSN", "")
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

// Generated packages import each other, so those imports must spell the HOST
// module's path — the module being generated into, not storm's.
//
// This shipped wrong in v0.1.0 and no test caught it, because every
// generation that ever ran was inside this repository, where storm's own
// module path happens to be the right answer. A user's first `generate`
// emitted `github.com/gsoultan/storm/internal/store/user` into their module
// and produced code that could not compile. So the test generates into a
// module with a DIFFERENT path, which is the only arrangement that can tell
// the two apart.
func TestGenerate_ImportsTheHostModuleNotStorm(t *testing.T) {
	withModels(t, testmodel.All())

	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"),
		[]byte("module example.com/someoneelse\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	// A relative path, the way a user in their own module types it.
	if err := Run([]string{"generate", filepath.Join("internal", "store")}); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(tmp, "internal", "store", "store.gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"example.com/someoneelse/internal/store/`) {
		t.Fatalf("the context package does not import the host module; it says:\n%s",
			firstImports(string(src)))
	}
	if strings.Contains(string(src), `"github.com/gsoultan/storm/internal/store/`) {
		t.Fatal("generated code imports storm's own module path for the user's packages")
	}
	// storm's runtime is still imported from storm, which is the other half.
	if !strings.Contains(string(src), `"github.com/gsoultan/storm/runtime"`) {
		t.Fatal("the runtime import should still point at storm")
	}
}

func firstImports(src string) string {
	i := strings.Index(src, "import (")
	if i < 0 {
		return src[:200]
	}
	j := strings.Index(src[i:], ")")
	return src[i : i+j+1]
}

type prefixed struct {
	storm.Model
	Body string
}

func (m *prefixed) Schema(t *storm.Table) { t.Index(storm.Prefix(&m.Body, 191)) }

// A prefix length is what MySQL needs to index a TEXT column and what
// PostgreSQL has no way to say. Emitting the index without it would be a
// different index; the PostgreSQL commands refuse the model instead, naming
// the expression that means the same thing.
func TestCLI_RefusesMySQLOnlyIndexFactsForPostgres(t *testing.T) {
	withModels(t, []any{&prefixed{}})
	err := run([]string{"ddl"})
	if err == nil {
		t.Fatal("ddl emitted a PostgreSQL index for a MySQL prefix declaration")
	}
	for _, want := range []string{"prefix", "left(%s, 191)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

// The dialect has to be reachable from the COMMAND LINE, not just from
// codegen.PackageOptions. Every engine-specific piece was built and none of it
// could be asked for: `storm generate` always emitted PostgreSQL.
func TestCLI_DialectReachesTheGenerator(t *testing.T) {
	// NOT testmodel.All(): it carries a partial index, which MySQL has not, so
	// the check refuses it before the generator runs — correctly, and that is a
	// different test. This one is about whether the flag reaches the generator.
	withModels(t, []any{&portableUser{}})
	for _, tc := range []struct {
		dialect  string
		want     []string
		unwanted []string
	}{
		{"postgres", []string{`"portable_users"`, "$1"}, []string{"`portable_users`"}},
		{"mysql", []string{"`portable_users`", "?"}, []string{`\"portable_users\"`}},
		{"mariadb", []string{"`portable_users`", "?"}, []string{`\"portable_users\"`}},
	} {
		t.Run(tc.dialect, func(t *testing.T) {
			dir := filepath.Join(moduleScratch(t, "clidia"+tc.dialect), "store")
			if err := run([]string{"generate", "-dialect", tc.dialect, dir}); err != nil {
				t.Fatal(err)
			}
			src := readTree(t, dir)
			for _, w := range tc.want {
				if !strings.Contains(src, w) {
					t.Errorf("the generated %s package never contains %q", tc.dialect, w)
				}
			}
			for _, u := range tc.unwanted {
				if strings.Contains(src, u) {
					t.Errorf("the generated %s package still contains %q", tc.dialect, u)
				}
			}
		})
	}
}

// `storm ddl -dialect` must emit the DDL the chosen engine accepts, including
// the one clause the two MySQL-family servers disagree about.
func TestCLI_DDLPerDialect(t *testing.T) {
	withModels(t, []any{&portableUser{}})
	for _, tc := range []struct{ dialect, want string }{
		{"postgres", `CREATE TABLE "portable_users"`},
		{"mysql", "CREATE TABLE `portable_users`"},
		{"mariadb", "CREATE TABLE `portable_users`"},
	} {
		t.Run(tc.dialect, func(t *testing.T) {
			out := captureStdout(t, func() {
				if err := run([]string{"ddl", "-dialect", tc.dialect}); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, tc.want) {
				t.Errorf("ddl -dialect %s does not contain %q:\n%s", tc.dialect, tc.want, out)
			}
		})
	}
}

// An unknown dialect must name the ones storm knows, rather than silently
// generating PostgreSQL — which is the failure mode that makes a default
// dangerous.
func TestCLI_UnknownDialectIsRefused(t *testing.T) {
	withModels(t, testmodel.All())
	err := run([]string{"ddl", "-dialect", "sqlite"})
	if err == nil {
		t.Fatal("an unknown dialect was accepted")
	}
	for _, want := range []string{"sqlite", "postgres", "mysql", "mariadb"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q, got: %v", want, err)
		}
	}
}

// The commands that read a live PostgreSQL catalogue have no MySQL form, and
// must say so instead of connecting and failing somewhere deep.
func TestCLI_CatalogueCommandsRefuseANonPostgresDialect(t *testing.T) {
	withModels(t, testmodel.All())
	for _, cmd := range []string{"diff", "verify", "explain", "import"} {
		err := run([]string{cmd, "-dialect", "mysql", "x"})
		if err == nil {
			t.Fatalf("storm %s -dialect mysql was accepted", cmd)
		}
		if !strings.Contains(err.Error(), "PostgreSQL") {
			t.Errorf("storm %s: the refusal does not say why: %v", cmd, err)
		}
	}
}

// portableUser is a model with nothing in it that only one engine has, so a
// test about the DIALECT FLAG is not also a test about portability.
type portableUser struct {
	storm.Model
	Email string
	Name  string
}

func (u *portableUser) Schema(t *storm.Table) {
	t.Col(&u.Email).Size(320)
	t.Col(&u.Name).Size(120)
	t.Unique(&u.Email)
}

func readTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		b.Write(src)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.Len() == 0 {
		t.Fatalf("no Go files under %s", dir)
	}
	return b.String()
}
