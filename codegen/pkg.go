package codegen

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gsoultan/raorm/schema"
)

// PackageOptions configures a whole-context generation.
type PackageOptions struct {
	// Dir is the output root. One directory per table is created under it.
	Dir string

	// Import is the module path of raorm, e.g. "github.com/gsoultan/raorm".
	Import string

	// Only restricts generation to these table names. Empty means every table
	// in the schema.
	Only []string

	// OrderBy overrides the default ordering, keyed by table name. A table
	// absent from the map orders by its primary key.
	OrderBy map[string]string
}

// Package renders one Go package per table and returns path → contents, with
// paths relative to Dir. Nothing is written; the caller decides that, so a
// generation that fails partway cannot leave a half-written tree behind.
//
// One package per table, not one package holding every table. That is what
// docs/API.md describes — `user.Row`, `user.Query()`, `user.ID` — and it is
// what keeps generated code readable: inside its own package a table's type is
// just Row, so nothing needs a UserRow / OrgRow prefix. Relations and named
// plans live in the parent package, which imports the table packages; since a
// table package never imports a sibling, a has-many in both directions cannot
// produce an import cycle.
func Package(s *schema.Schema, o PackageOptions) (map[string][]byte, error) {
	if o.Import == "" {
		return nil, fmt.Errorf("codegen: PackageOptions.Import is required")
	}

	names := o.Only
	if len(names) == 0 {
		for _, t := range s.Tables {
			names = append(names, t.Name)
		}
	}
	sort.Strings(names) // determinism does not depend on model declaration order

	out := make(map[string][]byte, len(names))
	seen := make(map[string]string, len(names))
	for _, name := range names {
		t := s.Table(name)
		if t == nil {
			return nil, fmt.Errorf("codegen: no table %q in the model", name)
		}
		pkg, err := PackageName(t.GoName, t.Name)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[pkg]; dup {
			// Two tables landing in one directory would silently overwrite
			// each other; better to name the collision.
			return nil, fmt.Errorf(
				"codegen: tables %q and %q both generate package %q — rename one with t.Name(...)",
				prev, name, pkg)
		}
		seen[pkg] = name

		src, err := File(s, Options{
			Package: pkg,
			Import:  o.Import,
			Table:   name,
			OrderBy: o.OrderBy[name],
		})
		if err != nil {
			return nil, err
		}
		out[filepath.Join(pkg, pkg+".gen.go")] = src
	}
	return out, nil
}

// PackageName is the Go package a table generates into: the model type in
// lower case, so table "users" (from type User) becomes package "user".
//
// It uses the Go name rather than de-pluralising the table name, because
// pluralisation is not invertible — "users" → "user" is easy, "addresses" →
// "addres" is wrong, and English has no rule to appeal to. A table from
// introspection has no Go name, so it falls back to the table name and says so
// if that is not a usable identifier.
func PackageName(goName, table string) (string, error) {
	src := goName
	if src == "" {
		src = table
	}
	pkg := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return -1 // _ and - are legal in a path but not idiomatic in a package name
		}
	}, src)
	if pkg == "" || (pkg[0] >= '0' && pkg[0] <= '9') {
		return "", fmt.Errorf(
			"codegen: table %q has no usable package name (from %q) — set one with t.Name(...)",
			table, src)
	}
	if isGoKeyword(pkg) {
		return "", fmt.Errorf(
			"codegen: table %q generates package %q, which is a Go keyword — rename the model or the table",
			table, pkg)
	}
	return pkg, nil
}

func isGoKeyword(s string) bool {
	switch s {
	case "break", "case", "chan", "const", "continue", "default", "defer", "else",
		"fallthrough", "for", "func", "go", "goto", "if", "import", "interface",
		"map", "package", "range", "return", "select", "struct", "switch", "type", "var":
		return true
	}
	return false
}
