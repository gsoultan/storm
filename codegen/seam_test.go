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
			// Two kinds of literal are prose, not SQL being written, and both
			// are identified by a prefix the codebase already uses:
			//
			//   "//"      a comment emitted into generated code. Explaining
			//             that an UPDATE's identity is its column set is
			//             exactly what the output should carry.
			//   "raorm: " an error message. One of them says `After() needs
			//             every ORDER BY term in the same direction` — it is
			//             naming the caller's own clause back to them, and
			//             rewording it to dodge a regex makes the error worse.
			//
			// Neither exemption can hide a real leak: a SELECT this generator
			// emits is a bare fragment, and prefixing it with either of these
			// would not compile.
			trimmed := strings.TrimSpace(v)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "raorm: ") {
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
