package tooldiscover

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

// Fingerprint summarises every Go file discovery would read.
//
// It is what a watcher compares between polls. The same walk rules as Discover
// are used deliberately: watching a file discovery ignores would regenerate for
// no reason, and ignoring one it reads would miss a model.
//
// exclude is the generated output directory. It must be skipped or the
// generator's own writes look like a change and the watcher never settles.
func Fingerprint(root, exclude string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	var absExclude string
	if exclude != "" {
		if absExclude, err = filepath.Abs(exclude); err != nil {
			return "", err
		}
	}

	h := sha256.New()
	err = filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != absRoot && skipDir(p, d.Name()) {
				return fs.SkipDir
			}
			if absExclude != "" && p == absExclude {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// A file that vanished between the walk and the stat is a save in
			// progress, not an error. The next poll sees the result.
			return nil
		}
		rel, err := filepath.Rel(absRoot, p)
		if err != nil {
			return err
		}
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write([]byte(strconv.FormatInt(info.Size(), 10)))
		h.Write([]byte{0})
		h.Write([]byte(strconv.FormatInt(info.ModTime().UnixNano(), 10)))
		h.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
