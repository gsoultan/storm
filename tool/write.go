package tool

import (
	"bytes"
	"os"
	"path/filepath"
)

// writeGenerated writes one generated file, and only when its bytes differ
// from what is already there.
//
// Unchanged files are left alone so their modification time does not move:
// under `storm watch` every save regenerates the whole tree, and rewriting
// identical files made gopls reload every table package and restarted any
// live-reload tool watching the module. The write itself goes to a temporary
// file in the same directory and is renamed over the target, so a run that
// dies part way leaves each file either old or new, never half written.
func writeGenerated(path string, data []byte) (changed bool, err error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has happened
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	// CreateTemp makes the file 0600; generated code is ordinary source.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}
