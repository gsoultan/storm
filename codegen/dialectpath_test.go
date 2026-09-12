package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// A generator carries two halves of the dialect seam: the decoder family and
// the query lowering. They were assigned separately, at four call sites, and
// three of them set only the decoders — so a context file's unions rendered
// through a ZERO lowering whose function fields are nil.
//
// It cost an afternoon to find because the symptom was a hang inside a call
// that read as though it were PostgreSQL's. The lesson is the one the MySQL
// work keeps re-teaching: when a type gains a field that must be initialised,
// the constructors that already existed do not gain it with it.
//
// So: setDialect resolves one and inheritDialect copies one, and this fails if
// anywhere else touches either half — which is how the fifth site was found,
// a scratch generator that copied the decoders and left the lowering zero.
func TestOnlySetDialectResolvesADialect(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range as.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "dec" && sel.Sel.Name != "lw") {
					continue
				}
				pos := fset.Position(as.Pos())
				// setDialect is the one place allowed to, and it must set both.
				if fn := enclosingFunc(af, fset, as.Pos()); fn == "setDialect" || fn == "inheritDialect" {
					continue
				}
				offenders = append(offenders,
					pos.String()+" assigns ."+sel.Sel.Name+" outside setDialect")
			}
			return true
		})
	}
	for _, o := range offenders {
		t.Errorf("%s\n  a generator with half a dialect is not a state worth being able "+
			"to express; call g.setDialect so the two cannot drift apart", o)
	}
}

func enclosingFunc(af *ast.File, fset *token.FileSet, pos token.Pos) string {
	name := ""
	ast.Inspect(af, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if ok && fd.Pos() <= pos && pos <= fd.End() {
			name = fd.Name.Name
		}
		return true
	})
	return name
}

// The other half of the seam, and the half that leaked: naming a dialect's
// package DIRECTLY rather than going through the lowering.
//
// TestOnlySetDialectResolvesADialect checks that the lowering is ASSIGNED in
// one place. It cannot see a call that skips it — and havingSpecs called
// pgsql.ExistsOpen unconditionally, so the semi-join in a MySQL package came
// out with double-quoted identifiers. That is SQL which parses nowhere, and it
// survived every unit test, the golden tests and both shell gates: the context
// file is generated once per package rather than per table, so it kept a
// hard-coded dialect while everything generated per table went through the
// seam.
//
// So every direct reference to a dialect package is listed here. Adding one is
// a decision to be argued in this table, not a line to be slipped in.
func TestEveryDialectReferenceIsAccountedFor(t *testing.T) {
	// allowed maps a file to the dialect symbols it may name directly.
	//
	// lowering.go is the seam itself — it exists to name them. The rest are
	// capabilities storm treats as PostgreSQL's vocabulary rather than as a
	// spelling: an upsert's conflict target and a lock's Go method name are the
	// same on every back end that HAS them, and a back end that has not is
	// refused or skipped, not respelled.
	allowed := map[string]map[string]bool{
		"lowering.go": nil, // the seam: anything
		"write.go": {
			"ConflictKey": true, "ConflictSpec": true, "ConflictDoNothing": true,
			"ConflictDoUpdate": true, "ExcludedAssign": true, "ConflictAny": true,
		},
		"softdelete.go": {"Live": true},
		"gen.go":        {"Live": true},
		"topn.go":       {"OrderSep": true},
		// ColumnCase turns a declared field name into a column name. The
		// convention is storm's, not a dialect's — every back end has to agree
		// on it or a projection's alias would differ between targets.
		"union.go":     {"ColumnCase": true},
		"join.go":      {"Live": true, "ColumnCase": true},
		"aggregate.go": {"Live": true, "ColumnCase": true},
		// The traversal directions, numbered once for the same reason the lock
		// modes are: the number is an index into a generated array, so it
		// belongs to the generated API rather than to a back end.
		"recursive.go": {"Live": true, "Ascend": true, "Descend": true},
		// `storm explain` reads a live PostgreSQL query plan. It has no form
		// on another engine and the CLI refuses one, so the whole command is
		// PostgreSQL's by construction rather than by omission.
		"explain.go": nil,
	}

	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		ok, listed := allowed[filepath.Base(f)]
		if listed && ok == nil {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			sel, is := n.(*ast.SelectorExpr)
			if !is {
				return true
			}
			pkg, is := sel.X.(*ast.Ident)
			if !is {
				return true
			}
			switch pkg.Name {
			case "pgsql", "mysql", "mariadb", "pgddl", "myddl":
			default:
				return true
			}
			if ok[sel.Sel.Name] {
				return true
			}
			t.Errorf("%s:%d names %s.%s directly. Route it through the lowering, or "+
				"add it to this test's table with the reason it is dialect-independent.",
				filepath.Base(f), fset.Position(sel.Pos()).Line, pkg.Name, sel.Sel.Name)
			return true
		})
	}
}
