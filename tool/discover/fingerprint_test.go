package tooldiscover

import (
	"os"
	"path/filepath"
	"testing"
)

// The watcher compares fingerprints, so two properties matter: it must be
// stable when nothing changed (or the watcher regenerates forever) and it must
// move when a watched file did (or a save is missed).

func TestFingerprintIsStable(t *testing.T) {
	dir := filepath.Join("testdata", "basic")
	first, err := Fingerprint(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := Fingerprint(dir, "")
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("run %d differs with no change; the watcher would regenerate forever", i)
		}
	}
}

func TestFingerprintMovesOnEdit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/f\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "m.go")
	if err := os.WriteFile(src, []byte("package m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := Fingerprint(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("package m\n\ntype T struct{ A int }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := Fingerprint(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("an edit did not move the fingerprint; the watcher would miss the save")
	}
}

// The generated tree must be excluded, or the generator's own writes look like
// a change and the watcher never settles.
func TestFingerprintExcludesTheOutputDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/f\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte("package m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "store")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	before, err := Fingerprint(dir, out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "store.gen.go"), []byte("package store\n\nvar X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := Fingerprint(dir, out)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("writing into the output directory moved the fingerprint; the watcher would loop on its own output")
	}
}

// A file that does not parse hides every model in it. Reporting "no models
// found" would send the developer looking for a missing declaration rather
// than the typo they just made.
func TestUnparsedFilesAreReported(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/broken\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "m.go"), []byte("package m\n\nthis is not go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Unparsed) == 0 {
		t.Fatal("a file that does not parse was skipped silently")
	}
}

// writeTree writes files under dir, creating directories as needed.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Bumping storm in go.mod changes what generate emits without touching a Go
// file, so the watcher has to see it.
func TestSnapshotSeesGoModAndGoSum(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"go.mod": "module example.com/f\n\ngo 1.27\n",
		"m.go":   "package m\n",
	})
	before, err := TakeSnapshot(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, map[string]string{"go.sum": "example.com/x v1.0.0 h1:abc=\n"})
	after, err := TakeSnapshot(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Changed(before); len(got) != 1 || got[0] != "go.sum" {
		t.Fatalf("Changed = %v, want [go.sum]", got)
	}
}

// What the watcher regenerates for, and what it does not. An edit to a
// handler used to cost a full go run of the bootstrap.
func TestWatchSetRelevance(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.27\n",
		"model/user.go": `package model

import (
	"github.com/gsoultan/storm"
	"example.com/app/shared"
)

type User struct {
	storm.Model
	shared.Audit
	Name string
}
`,
		"shared/audit.go": "package shared\n\ntype Audit struct{ By string }\n",
		"api/handler.go":  "package api\n\nfunc Handle() {}\n",
	})
	r, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Models) != 1 {
		t.Fatalf("discovered %d models, want 1", len(r.Models))
	}
	ws := r.WatchSet()

	// A brand-new model package is found by its storm import.
	writeTree(t, dir, map[string]string{
		"billing/invoice.go": "package billing\n\nimport \"github.com/gsoultan/storm\"\n\ntype Invoice struct{ storm.Model }\n",
	})
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"model/user.go", true},
		{"shared/audit.go", true}, // a mixin's columns live where it is declared
		{"go.mod", true},
		{"billing/invoice.go", true},
		{"api/handler.go", false},
		{"model/user_test.go", false},
		{"api/gone.go", false}, // deleted, outside every model package
	} {
		if got := ws.Relevant([]string{tc.file}); got != tc.want {
			t.Errorf("Relevant(%s) = %v, want %v", tc.file, got, tc.want)
		}
	}

	var none *WatchSet
	if !none.Relevant([]string{"api/handler.go"}) {
		t.Error("with no discovery to go on, a change must regenerate")
	}
}
