// Package codegenmongo generates the MongoDB back end's table packages
// (ADR-0004).
//
// It is not codegen with a flag. codegen's lowering is SQL punctuation from
// end to end, and a document back end shares the plan, not the text: the
// token stream, the shape cache, the Row type and the method names are the
// same, and every statement is a command document that compile/mongo wrote.
// The surface is the SQL packages' core — typed predicates, ordering, paging,
// All/Each/One/Count/Exists, Create/Insert, Mutate/Update, Delete, UpdateAll,
// DeleteAll, InsertAll — so a call site reads the same against either store.
// What has no faithful MongoDB form is refused by compile/mgddl before
// anything is written, naming the construct.
package codegenmongo

import (
	"fmt"
	"go/format"
	"path/filepath"
	"sort"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/compile/mgddl"
	"github.com/gsoultan/storm/schema"
)

// Options configures a generation.
type Options struct {
	// Import is storm's module path, e.g. "github.com/gsoultan/storm".
	Import string
	// Only restricts generation to these tables; empty means all.
	Only []string
	// Package, when set, also renders the context package: <Package>.gen.go
	// at the output root, holding a plan per relation and per declared plan.
	// PackageImport is its import path's parent — where the table packages
	// are — since an import path cannot be derived from a directory.
	Package, PackageImport string
}

// Package renders one Go package per table and returns path → contents,
// relative to the output root — the shape codegen.Package returns, so the
// caller writes either the same way.
func Package(s *schema.Schema, o Options) (map[string][]byte, error) {
	if o.Import == "" {
		return nil, fmt.Errorf("codegenmongo: Options.Import is required")
	}
	if err := mgddl.Check(s); err != nil {
		return nil, err
	}
	names := o.Only
	if len(names) == 0 {
		for _, t := range s.Tables {
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	out := map[string][]byte{}
	seen := map[string]string{}
	for _, name := range names {
		t := s.Table(name)
		if t == nil {
			return nil, fmt.Errorf("codegenmongo: no table %q in the model", name)
		}
		pkg, err := codegen.PackageName(t.GoName, t.Name)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[pkg]; dup {
			return nil, fmt.Errorf("codegenmongo: tables %q and %q both generate package %q", prev, name, pkg)
		}
		seen[pkg] = name
		src, err := File(s, t, pkg, o.Import)
		if err != nil {
			return nil, err
		}
		out[filepath.Join(pkg, pkg+".gen.go")] = src
	}
	if o.Package != "" {
		if o.PackageImport == "" {
			return nil, fmt.Errorf("codegenmongo: Options.Package is %q but PackageImport is empty", o.Package)
		}
		src, err := Context(s, names, o.Package, o.PackageImport, o.Import)
		if err != nil {
			return nil, err
		}
		out[o.Package+".gen.go"] = src
	}
	return out, nil
}

// File renders one table's package. s is the whole model, which a delete's
// referential actions are read from; nil generates none.
func File(s *schema.Schema, t *schema.Table, pkg, imp string) ([]byte, error) {
	g, err := newGen(s, t, pkg, imp)
	if err != nil {
		return nil, err
	}
	g.emit()
	src, err := format.Source(g.b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("codegenmongo: %s: generated source does not parse (a generator bug): %w\n%s",
			t.Name, err, g.b.Bytes())
	}
	return src, nil
}
