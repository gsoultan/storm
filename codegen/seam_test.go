package codegen_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// sqlText is SQL syntax as this codebase writes it: keywords upper case. It
// deliberately does not match Go identifiers like `selectPrefix` or `Limit`,
// because those are the *names* of lowered fragments and belong here.
var sqlText = regexp.MustCompile(`\b(SELECT|INSERT +INTO|UPDATE|DELETE +FROM|FROM|WHERE|ORDER +BY|GROUP +BY|HAVING|LIMIT|OFFSET|JOIN|LATERAL|UNION|RETURNING|ON +CONFLICT|VALUES|IS +NULL|IS +NOT +NULL|LIKE|ANY\(|count\(\*\))\b`)

// TestNoSQLTextInCodegen is risk R9's enforcement.
//
// Every SELECT keyword, identifier quote and `$` placeholder for the read path
// lives in compile/pgsql. codegen/ asks for strings and emits them. The seam is
// invisible while Postgres is the only back end, so nothing but a check stops
// it rotting shut before M9 has a second one to prove it — and by then every
// feature built on top has to be unpicked.
//
// If this fails: the SQL belongs in compile/pgsql, and codegen should call it.
func TestNoSQLTextInCodegen(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Comments are excluded on purpose: explaining a lowering is not
		// performing one, and the explanations are worth keeping.
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			// A literal that starts with `//` is a comment being emitted into
			// generated code, not SQL being written. Explaining that an
			// UPDATE's identity is its column set is exactly the kind of
			// comment the generated output should carry, and rewording it to
			// dodge a regex would make the output worse to read.
			if strings.HasPrefix(strings.TrimSpace(v), "//") {
				return true
			}
			if m := sqlText.FindString(v); m != "" {
				t.Errorf("%s: SQL text %q in a string literal:\n  %s\n"+
					"  move it to compile/pgsql and call it from here",
					fset.Position(lit.Pos()), m, v)
			}
			return true
		})
	}
}
