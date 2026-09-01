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
