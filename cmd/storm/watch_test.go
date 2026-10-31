package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gsoultan/storm/tool/discover"
)

// A save made after the baseline was taken must still be reported, however
// long the generate in between took. The watcher used to re-read the tree
// after generating and adopt that as its baseline, which swallowed exactly
// this save.
func TestSettleReportsASaveMadeAfterTheBaseline(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/w\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "m.go")
	if err := os.WriteFile(src, []byte("package m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "store")
	last, err := tooldiscover.TakeSnapshot(dir, out)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := settle(dir, out, last); ok {
		t.Fatal("settle reported a change on an untouched tree")
	}

	// The save lands while "generate" runs; the baseline stays the one taken
	// before it, as watch now keeps it.
	if err := os.WriteFile(src, []byte("package m\n\ntype T struct{ A int }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// And the generator's own output must not count.
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "store.gen.go"), []byte("package store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, changed, ok := settle(dir, out, last)
	if !ok || len(changed) != 1 || changed[0] != "m.go" {
		t.Fatalf("settle = %v, %v; want [m.go], true", changed, ok)
	}
}
