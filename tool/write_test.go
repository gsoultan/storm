package tool

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An identical regenerate must leave the file's modification time alone, or
// every save under `storm watch` reloads every table package in the editor.
func TestWriteGeneratedSkipsIdenticalBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.gen.go")
	if changed, err := writeGenerated(p, []byte("package x\n")); err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if changed, err := writeGenerated(p, []byte("package x\n")); err != nil || changed {
		t.Fatalf("identical write: changed=%v err=%v", changed, err)
	}
	if fi, _ := os.Stat(p); !fi.ModTime().Equal(old) {
		t.Fatalf("identical bytes moved the mtime to %v", fi.ModTime())
	}

	if changed, err := writeGenerated(p, []byte("package x\n\nvar V = 1\n")); err != nil || !changed {
		t.Fatalf("changed write: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "package x\n\nvar V = 1\n" {
		t.Fatalf("content = %q", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", fi.Mode().Perm())
	}
	// Nothing left behind by the temp-and-rename.
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Fatalf("directory holds %d entries, want 1", len(ents))
	}
}
