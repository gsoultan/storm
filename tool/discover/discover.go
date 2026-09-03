// Package tooldiscover finds an adopter's models in their source, so the tool
// does not have to be handed them.
//
// storm resolves field pointers by offset, which is a runtime operation, so
// the generator has always needed to LINK against the models rather than read
// them — hence the bootstrap main every adopter used to write. That
// requirement is real, but it only ever needed one static answer: which types
// are models, and where do they live. This package answers exactly that, by
// parsing, and nothing else. Everything semantic still happens by running the
// user's code.
//
// Discovery is syntactic on purpose. Type-checking the adopter's module would
// mean depending on golang.org/x/tools and failing whenever their code does
// not compile — including the very first run, before any store exists to
// compile against.
package tooldiscover

import (
	"fmt"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Skipped is a declaration that matched a rule but did not become a model.
type Skipped struct {
	ImportPath string
	Name       string
	Pos        string
	Why        string
	// Actionable separates "you probably meant this to be a table" from "this
	// is a mixin, which is how mixins are supposed to look". Only the first
	// kind is worth printing on every command; a warning that fires on correct
	// code is one people learn to scroll past.
	Actionable bool
}

// Discover walks the module containing start and returns its models.
func Discover(start string) (*Result, error) {
	mod, err := FindModule(start)
	if err != nil {
		return nil, err
	}
	r := &Result{Module: mod}
	fset := token.NewFileSet()
	var inMain []string

	// Judged in two passes. Whether a type is a mixin depends on whether some
	// OTHER package embeds it, which is not known until the whole module has
	// been read — so nothing is decided during the walk.
	type scanned struct {
		scan *pkgScan
		ip   string
	}
	var all []scanned
	embedded := map[string]bool{}

	err = filepath.WalkDir(mod.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if p != mod.Root && skipDir(p, d.Name()) {
			return fs.SkipDir
		}
		scan, err := scanDir(fset, p)
		if err != nil || scan == nil {
			return err
		}
		ip, err := mod.ImportPath(p)
		if err != nil {
			return err
		}
		for name := range scan.localEmbeds {
			embedded[ip+"."+name] = true
		}
		for q := range scan.qualEmbeds {
			embedded[q] = true
		}
		r.Unparsed = append(r.Unparsed, scan.unparsed...)
		all = append(all, scanned{scan, ip})
		return nil
	})
	if err != nil {
		return nil, err
	}

	for _, s := range all {
		// An embedded type is a mixin, not a model: it contributes columns to
		// whatever embeds it and has no table of its own.
		for _, set := range []map[string]token.Position{s.scan.structs, s.scan.unexported} {
			for name, pos := range set {
				if !embedded[s.ip+"."+name] {
					continue
				}
				delete(set, name)
				// Not Actionable: an embedded type is a mixin, and a mixin is
				// exactly what correct code looks like. Warning here would fire
				// on every run of a module that uses them properly.
				r.Skipped = append(r.Skipped, Skipped{
					ImportPath: s.ip, Name: name, Pos: pos.String(),
					Why: "embedded in another struct, so it is a mixin rather than a table",
				})
			}
		}
		// A model in package main cannot be imported by anything, so no
		// bootstrap can reach it. This used to surface as a confusing failure
		// inside generated code; name it here instead.
		if s.scan.name == "main" && len(s.scan.structs) > 0 {
			for name := range s.scan.structs {
				inMain = append(inMain, s.ip+"."+name)
			}
			continue
		}
		collect(r, s.scan, s.ip)
	}
	if len(inMain) > 0 {
		sort.Strings(inMain)
		return nil, fmt.Errorf(
			"model(s) declared in package main and unreachable: %s\n"+
				"       package main cannot be imported, so nothing can register them —\n"+
				"       move them to their own package (a `model` package is the convention)",
			strings.Join(inMain, ", "))
	}

	// Deterministic order. Generated output must be byte-identical across runs
	// and machines, and map iteration is neither.
	sort.Slice(r.Models, func(i, j int) bool {
		if r.Models[i].ImportPath != r.Models[j].ImportPath {
			return r.Models[i].ImportPath < r.Models[j].ImportPath
		}
		return r.Models[i].TypeName < r.Models[j].TypeName
	})
	sort.Slice(r.Undeclarable, func(i, j int) bool {
		return r.Undeclarable[i].Pos < r.Undeclarable[j].Pos
	})
	sort.Slice(r.Queries, func(i, j int) bool {
		if r.Queries[i].ImportPath != r.Queries[j].ImportPath {
			return r.Queries[i].ImportPath < r.Queries[j].ImportPath
		}
		return r.Queries[i].VarName < r.Queries[j].VarName
	})
	sort.Slice(r.Skipped, func(i, j int) bool {
		if r.Skipped[i].ImportPath != r.Skipped[j].ImportPath {
			return r.Skipped[i].ImportPath < r.Skipped[j].ImportPath
		}
		return r.Skipped[i].Name < r.Skipped[j].Name
	})
	return r, nil
}

func collect(r *Result, scan *pkgScan, ip string) {
	for name, pos := range scan.structs {
		r.Models = append(r.Models, Model{
			ImportPath: ip,
			PkgName:    scan.name,
			TypeName:   name,
			Pos:        pos.String(),
			Why:        scan.reasons[name],
		})
	}
	for name, pos := range scan.queries {
		r.Queries = append(r.Queries, Query{
			ImportPath: ip,
			PkgName:    scan.name,
			VarName:    name,
			Pos:        pos.String(),
		})
	}
	r.Undeclarable = append(r.Undeclarable, scan.undeclarable...)
	// Unexported matches are reported, not failed — erroring would break
	// mixins, which are a supported shape, to catch a mistake.
	for name, pos := range scan.unexported {
		r.Skipped = append(r.Skipped, Skipped{
			ImportPath: ip, Name: name, Pos: pos.String(),
			Why:        "unexported, so no generated bootstrap can name it — export it to make it a table",
			Actionable: true,
		})
	}
	for name, pos := range scan.unexportedQ {
		r.Skipped = append(r.Skipped, Skipped{
			ImportPath: ip, Name: name, Pos: pos.String(),
			Why:        "unexported raw query — export it, or it gets no generated scanner and fails at the first call",
			Actionable: true,
		})
	}
}

func skipDir(path, name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	if name == "vendor" || name == "testdata" || name == "node_modules" {
		return true
	}
	// A nested module is somebody else's module, with its own go.mod and its
	// own storm invocation if it wants one.
	if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
		return true
	}
	return false
}
