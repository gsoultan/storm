package tooldiscover

import (
	"crypto/sha256"
	"encoding/hex"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Fingerprint summarises every Go file discovery would read, plus the
// module's go.mod and go.sum.
//
// It is what a watcher compares between polls. The same walk rules as Discover
// are used deliberately: watching a file discovery ignores would regenerate for
// no reason, and ignoring one it reads would miss a model. go.mod and go.sum
// are in it because bumping storm's version changes what generate emits while
// touching no Go file at all.
//
// exclude is the generated output directory. It must be skipped or the
// generator's own writes look like a change and the watcher never settles.
func Fingerprint(root, exclude string) (string, error) {
	s, err := TakeSnapshot(root, exclude)
	if err != nil {
		return "", err
	}
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(s[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Snapshot is Fingerprint itemised: one stamp (size and modification time) per
// watched file, keyed by its slash-separated path relative to the module root.
//
// A watcher needs the itemised form to answer the question the hash cannot:
// not whether something changed, but WHAT did, so an edit to an HTTP handler
// does not cost a full generate.
type Snapshot map[string]string

// TakeSnapshot walks root under the same rules as Fingerprint.
func TakeSnapshot(root, exclude string) (Snapshot, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var absExclude string
	if exclude != "" {
		if absExclude, err = filepath.Abs(exclude); err != nil {
			return nil, err
		}
	}

	s := Snapshot{}
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
		name := d.Name()
		top := filepath.Dir(p) == absRoot
		if !strings.HasSuffix(name, ".go") && !(top && (name == "go.mod" || name == "go.sum")) {
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
		s[filepath.ToSlash(rel)] = strconv.FormatInt(info.Size(), 10) + "/" +
			strconv.FormatInt(info.ModTime().UnixNano(), 10)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Changed lists the files added, removed or modified since prev, sorted.
func (s Snapshot) Changed(prev Snapshot) []string {
	var out []string
	for k, v := range s {
		if pv, ok := prev[k]; !ok || pv != v {
			out = append(out, k)
		}
	}
	for k := range prev {
		if _, ok := s[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// WatchSet decides which changed files can affect what generate emits.
//
// A file can, when it is in a package discovery found a model, query or union
// in, or in a module package one of those imports (a mixin's columns live in
// the package that declares it, not the one that embeds it). A file anywhere
// else can only start to matter by importing storm, which is how a NEW model
// package appears, so that one import is read from the changed file itself.
type WatchSet struct {
	root string
	dirs map[string]bool // slash-separated, module-relative; "." is the root
}

// WatchSet builds the set from a discovery result. The import closure is read
// with ImportsOnly parses of the packages already found, so it costs a
// fraction of the discovery that produced r.
func (r *Result) WatchSet() *WatchSet {
	w := &WatchSet{root: r.Module.Root, dirs: map[string]bool{}}
	fset := token.NewFileSet()
	queue := r.Packages()
	for len(queue) > 0 {
		ip := queue[0]
		queue = queue[1:]
		rel, ok := moduleRel(r.Module.Path, ip)
		if !ok || w.dirs[rel] {
			continue
		}
		w.dirs[rel] = true
		ents, err := os.ReadDir(filepath.Join(r.Module.Root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		for _, e := range ents {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(r.Module.Root, filepath.FromSlash(rel), n), nil, parser.ImportsOnly)
			if err != nil {
				continue
			}
			for _, imp := range f.Imports {
				queue = append(queue, strings.Trim(imp.Path.Value, `"`))
			}
		}
	}
	return w
}

// Relevant reports whether any of the changed files (as Snapshot.Changed
// returns them) can change what generate emits. A nil WatchSet, which is what
// a watcher holds after a discovery that failed, treats every change as
// relevant: with nothing known, regenerating is the only safe answer.
func (w *WatchSet) Relevant(changed []string) bool {
	if w == nil {
		return len(changed) > 0
	}
	for _, rel := range changed {
		if rel == "go.mod" || rel == "go.sum" {
			return true
		}
		if strings.HasSuffix(rel, "_test.go") {
			continue // discovery never reads tests
		}
		if w.dirs[path.Dir(rel)] {
			return true
		}
		if importsStorm(filepath.Join(w.root, filepath.FromSlash(rel))) {
			return true
		}
	}
	return false
}

// importsStorm reports whether the file imports storm itself. A file that no
// longer exists imports nothing; if it held a model, its directory was in the
// set and the deletion was already relevant.
func importsStorm(file string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		// Half-written mid-save, most likely. Regenerating costs seconds;
		// missing a new model costs a confused developer.
		_, statErr := os.Stat(file)
		return statErr == nil
	}
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) == stormPath {
			return true
		}
	}
	return false
}

// moduleRel maps an import path inside the module to its slash-separated
// directory relative to the module root.
func moduleRel(modPath, ip string) (string, bool) {
	if ip == modPath {
		return ".", true
	}
	if strings.HasPrefix(ip, modPath+"/") {
		return strings.TrimPrefix(ip, modPath+"/"), true
	}
	return "", false
}
