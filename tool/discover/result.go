package tooldiscover

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Result is everything a synthesized bootstrap needs to be written.
type Result struct {
	Module  *Module
	Models  []Model
	Queries []Query
	// Skipped is what matched a rule but cannot be reached. It is reported to
	// the developer rather than failed on, because the common cause is a
	// mixin, which is legitimate.
	Skipped []Skipped
	// Unparsed are files discovery could not read. Any model in them is
	// invisible, so this has to be said out loud before "no models found" is.
	Unparsed []string
}

// Packages is every import the bootstrap needs, deduplicated and ordered.
func (r *Result) Packages() []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range r.Models {
		if !seen[m.ImportPath] {
			seen[m.ImportPath] = true
			out = append(out, m.ImportPath)
		}
	}
	for _, q := range r.Queries {
		if !seen[q.ImportPath] {
			seen[q.ImportPath] = true
			out = append(out, q.ImportPath)
		}
	}
	return out
}

// ShimDir is where the synthesized bootstrap can live.
//
// It cannot simply be the module root. Go's internal rule says a package under
// `foo/internal/...` is importable only from within `foo/`, so a bootstrap at
// the root that imports `foo/internal/model` does not compile — and it fails
// inside a generated file the developer never asked for, which is the worst
// place for an error to appear. So the shim goes in the deepest directory that
// can see every discovered package, and if no such directory exists this says
// so in terms of the two packages that conflict.
func (r *Result) ShimDir() (string, error) {
	pkgs := r.Packages()
	if len(pkgs) == 0 {
		return "", nil
	}
	rels := make([][]string, 0, len(pkgs))
	for _, p := range pkgs {
		rels = append(rels, splitRel(r.Module.Path, p))
	}

	common := rels[0]
	for _, segs := range rels[1:] {
		common = commonPrefix(common, segs)
	}

	// Every `internal` element imposes a ceiling: the shim must sit at or
	// below the directory that CONTAINS it.
	for i, segs := range rels {
		for j, s := range segs {
			if s != "internal" || len(common) >= j {
				continue
			}
			// Name the package that actually breaks the rule — the first one
			// that does NOT live under this internal's parent — rather than
			// whichever other package happens to be next in the list.
			return "", fmt.Errorf(
				"no directory can import both %s and %s: the first is internal to %s, "+
					"which does not contain the second\n"+
					"       move the models into one tree, or keep a bootstrap main (docs/EXAMPLE.md §2)",
				pkgs[i], outsideOf(pkgs, rels, segs[:j], i), pathOf(r.Module.Path, segs[:j]))
		}
	}
	return filepath.Join(append([]string{r.Module.Root}, common...)...), nil
}

// splitRel is the module-relative path of an import, as segments. The root
// package is no segments rather than one empty one, so it is a prefix of
// everything and never matches a directory name.
func splitRel(modPath, importPath string) []string {
	rel := strings.TrimPrefix(strings.TrimPrefix(importPath, modPath), "/")
	if rel == "" {
		return nil
	}
	return strings.Split(rel, "/")
}

func pathOf(modPath string, segs []string) string {
	if len(segs) == 0 {
		return modPath
	}
	return modPath + "/" + path.Join(segs...)
}

// outsideOf names the first package that does not live under boundary, which
// is the one whose presence makes the internal package unreachable.
func outsideOf(pkgs []string, rels [][]string, boundary []string, skip int) string {
	for j := range pkgs {
		if j == skip {
			continue
		}
		if len(commonPrefix(rels[j], boundary)) < len(boundary) {
			return pkgs[j]
		}
	}
	return pkgs[skip]
}

func commonPrefix(a, b []string) []string {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}
