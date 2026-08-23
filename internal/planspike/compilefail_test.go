package planspike_test

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A compile error is only a guarantee if something checks that it still
// happens. Every file in testdata/compilefail must fail to build, with the
// message named in its `// want:` line — otherwise a refactor can quietly turn
// ADR-0003 from a type-system property back into a convention.
func TestCompileFail(t *testing.T) {
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
			want := wantMessage(t, c)

			rel := filepath.Join("internal", "cfail"+strconv.Itoa(os.Getpid())+strings.TrimSuffix(filepath.Base(c), ".go"))
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

// wantMessage reads the `// want: ...` line the case must fail with.
func wantMessage(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if s, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "// want:"); ok {
			return strings.TrimSpace(s)
		}
	}
	t.Fatalf("%s has no `// want:` line saying which error it must produce", path)
	return ""
}
