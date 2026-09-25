// Command archcheck enforces the structural rules in AGENTS.md that a grep
// cannot: they are about declarations, so they read the AST.
//
// AGENTS.md called every one of these rules CI-enforced for months while
// nothing enforced them, and the tree drifted from four of them. Where it
// drifted, the rule is held as a RATCHET rather than fixed in one sweep: the
// numbers below are what the tree had when the check was written, a
// violation may not grow past them, and one that shrinks must have its number
// lowered here so the next regression is caught at the new level. Splitting
// runtime/ to get under ten files would move exported types, which is itself a
// breaking change — the ratchet is the honest version of the rule.
//
// Scope is storm's own packages. bench/ (ent's generated fixtures among them),
// examples/ (adopter code) and internal/ (spikes and tools like this one) are
// not held to it, and neither is any directory with a go.mod of its own.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxFiles is the grandfathered file count of every folder that had more
// than ten Go files, test files excluded, when this check was written.
var maxFiles = map[string]int{
	".":              19,
	"codegen":        27,
	"compile/mssql":  11,
	"compile/oracle": 12,
	"migrate":        11,
	"runtime":        20,
	"runtime/msdrv":  12,
	"runtime/pgxdrv": 11,
}

// The same ratchet for "one struct per file" and "one interface per file",
// kept as one number each: the EXCESS, summed over files — a file with three
// structs contributes two. A single total catches a new offending file and a
// third struct added to an old one alike.
const (
	maxExtraStructs    = 134
	maxExtraInterfaces = 7
)

// maxMethods is AGENTS.md's interface budget, and executorBudget the port's.
const (
	maxMethods     = 15
	executorBudget = 5
)

type pkgFiles struct {
	clause string
	files  int
}

func main() {
	var problems []string
	fail := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	pkgs := map[string]*pkgFiles{}
	extraStructs, extraIfaces := 0, 0
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "." && skipDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution|parser.ParseComments)
		if err != nil {
			return err
		}
		// storm's OWN packages only. The rule rests on storm's clauses being
		// unique, which says nothing about the standard library's: a test in
		// storm's runtime package that needs Go's runtime has to name one of
		// the two.
		for _, im := range f.Imports {
			if im.Name != nil && im.Name.Name != "_" && strings.HasPrefix(im.Path.Value, `"github.com/gsoultan/storm`) {
				fail("%s imports %s as %s: AGENTS.md allows no import aliases of storm's own packages, "+
					"because every clause is unique and says what it is", path, im.Path.Value, im.Name.Name)
			}
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		p := pkgs[dir]
		if p == nil {
			p = &pkgFiles{clause: f.Name.Name}
			pkgs[dir] = p
		}
		p.files++

		structs, ifaces := 0, 0
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.TYPE {
				continue
			}
			for _, spec := range g.Specs {
				ts := spec.(*ast.TypeSpec)
				switch t := ts.Type.(type) {
				case *ast.StructType:
					structs++
				case *ast.InterfaceType:
					ifaces++
					n := methods(t)
					if n > maxMethods {
						fail("%s: interface %s has %d methods; the budget is %d", path, ts.Name.Name, n, maxMethods)
					}
					if dir == "runtime" && ts.Name.Name == "Executor" && n > executorBudget {
						fail("runtime.Executor has %d methods; the port's budget is %d", n, executorBudget)
					}
				}
			}
		}
		if structs > 1 {
			extraStructs += structs - 1
		}
		if ifaces > 1 {
			extraIfaces += ifaces - 1
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "archcheck:", err)
		os.Exit(2)
	}

	byClause := map[string][]string{}
	for dir, p := range pkgs {
		if p.clause != "main" {
			byClause[p.clause] = append(byClause[p.clause], dir)
		}
		limit, grandfathered := maxFiles[dir]
		if !grandfathered {
			limit = 10
		}
		switch {
		case p.files > limit:
			fail("%s has %d Go files; its limit is %d", dir, p.files, limit)
		case grandfathered && p.files < limit:
			fail("%s is down to %d Go files: lower its number in maxFiles to %d so it cannot grow back",
				dir, p.files, max(p.files, 10))
		}
	}
	for dir := range maxFiles {
		if pkgs[dir] == nil {
			fail("%s is in maxFiles but has no Go files: remove it", dir)
		}
	}
	for clause, dirs := range byClause {
		if len(dirs) > 1 {
			sort.Strings(dirs)
			fail("package %s is declared in %s: clauses must be unique, prefixed with their layer "+
				"where they would collide (schema/pg is package schemapg)", clause, strings.Join(dirs, " and "))
		}
	}
	ratchet(fail, "files declaring more than one struct", extraStructs, maxExtraStructs, "maxExtraStructs")
	ratchet(fail, "files declaring more than one interface", extraIfaces, maxExtraInterfaces, "maxExtraInterfaces")

	if len(problems) > 0 {
		sort.Strings(problems)
		for _, p := range problems {
			fmt.Println("  " + p)
		}
		os.Exit(1)
	}
	fmt.Println("OK")
}

func ratchet(fail func(string, ...any), what string, got, limit int, name string) {
	switch {
	case got > limit:
		fail("%s: %d extra declaration(s), up from %d — one type per file, as AGENTS.md says", what, got, limit)
	case got < limit:
		fail("%s: down to %d extra declaration(s) from %d — lower %s so it cannot grow back", what, got, limit, name)
	}
}

// methods counts an interface's own methods. An embedded interface is not
// counted: it is budgeted where it is declared.
func methods(t *ast.InterfaceType) int {
	n := 0
	for _, m := range t.Methods.List {
		if _, ok := m.Type.(*ast.FuncType); ok {
			n += len(m.Names)
		}
	}
	return n
}

func skipDir(path string) bool {
	base := filepath.Base(path)
	if base == "testdata" || strings.HasPrefix(base, ".") {
		return true
	}
	switch strings.SplitN(filepath.ToSlash(path), "/", 2)[0] {
	case "bench", "examples", "internal":
		return true
	}
	_, err := os.Stat(filepath.Join(path, "go.mod"))
	return err == nil
}
