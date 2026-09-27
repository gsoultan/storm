package tenants_test

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The sharding guarantee is a compile error, and a compile error is only a
// guarantee if something checks that it still happens. Every file in
// testdata/compilefail must fail to build against the committed generated
// package, with the message named in its `// want:` line. Otherwise a refactor
// of codegen's execType could quietly turn the check back into a convention.
//
// The harness is internal/planspike's, pointed at a sharded package.
func TestShardingCompileFail(t *testing.T) {
	if testing.Short() {
		t.Skip("builds each case; -short skips it")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cases, err := filepath.Glob(filepath.Join("testdata", "compilefail", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no compilefail cases — the guarantee is unasserted")
	}
	for _, c := range cases {
		t.Run(filepath.Base(c), func(t *testing.T) {
			want := wantLine(t, c)
			rel := filepath.Join("internal", "shfail"+strconv.Itoa(os.Getpid())+strings.TrimSuffix(filepath.Base(c), ".go"))
			dir := filepath.Join(root, rel)
			t.Cleanup(func() { os.RemoveAll(dir) })
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			src, err := os.ReadFile(c)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, filepath.Base(c)), src, 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("go", "build", "./"+filepath.ToSlash(rel))
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("%s COMPILED — it must not:\n%s", c, src)
			}
			if !strings.Contains(string(out), want) {
				t.Errorf("%s failed with the wrong error.\nwant it to mention: %q\ngot:\n%s", c, want, out)
			}
		})
	}
}

// wantLine reads the `// want: ...` line the case must fail with.
func wantLine(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if w, ok := strings.CutPrefix(sc.Text(), "// want: "); ok {
			return w
		}
	}
	t.Fatalf("%s has no `// want:` line", path)
	return ""
}
