package runtime

// The bug class -race cannot see.
//
// MaskCache published a mask and its statement as TWO atomics, written in
// sequence. Two goroutines warming different masks interleaved those writes
// and left the mask from one beside the statement from the other; the next hit
// on that mask returned an UPDATE compiled for a different column set, and the
// caller bound its arguments against those placeholders — wrong columns
// written, or a bind error, depending on the two masks.
//
// Every access was atomic, so it was never a data race. -race is blind to this
// by construction: it sees unsynchronised ACCESS, and every access here was
// synchronised. What was unsynchronised was the PAIRING, which no dynamic
// detector models.
//
// So the invariant is structural instead: one atomic field per struct. Two are
// two things that can be observed out of step, and the fix in both caches was
// the same — intern the pair and publish one pointer, which makes the mismatch
// unrepresentable rather than merely unlikely.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// twoAtomicsAreFine lists structs whose atomic fields are genuinely
// INDEPENDENT — two counters nobody reads together. Adding a name here is a
// claim that no reader ever needs the two values to agree, and that claim is
// the thing to argue about in review.
var twoAtomicsAreFine = map[string]bool{}

func TestNoStructPublishesAPairThroughTwoAtomics(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			var atomics []string
			for _, fld := range st.Fields.List {
				if !isAtomic(fld.Type) {
					continue
				}
				for _, nm := range fld.Names {
					atomics = append(atomics, nm.Name)
				}
				if len(fld.Names) == 0 {
					atomics = append(atomics, "<embedded>")
				}
			}
			if len(atomics) > 1 && !twoAtomicsAreFine[ts.Name.Name] {
				t.Errorf("%s:%d: %s has %d atomic fields (%s). Two atomics are two "+
					"things a reader can observe out of step, and -race cannot see "+
					"it because every access is synchronised — only the pairing is "+
					"not. Intern them in one struct and publish an "+
					"atomic.Pointer to it, or add %s to twoAtomicsAreFine with the "+
					"reason nobody reads them together.",
					filepath.Base(f), fset.Position(ts.Pos()).Line, ts.Name.Name,
					len(atomics), strings.Join(atomics, ", "), ts.Name.Name)
			}
			return true
		})
	}
}

// isAtomic reports whether a field's type is one of sync/atomic's — including
// the generic ones, whose syntax is an index expression rather than a plain
// selector.
func isAtomic(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.IndexExpr: // atomic.Pointer[T]
		return isAtomic(t.X)
	case *ast.IndexListExpr:
		return isAtomic(t.X)
	case *ast.StarExpr:
		return isAtomic(t.X)
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		return ok && pkg.Name == "atomic"
	}
	return false
}
