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
