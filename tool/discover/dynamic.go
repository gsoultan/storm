package tooldiscover

import (
	"go/ast"
	"go/token"
)

// Undeclarable is a storm.SQL or storm.SQLExec call the generator can never
// see, reported at generate time rather than left to fail at the first call.
//
// Discovery registers PACKAGE-LEVEL vars, and `storm generate` PREPAREs what
// it registers and emits a storm.RegisterStatement for each. A declaration
// built anywhere else — in a handler, in a helper, inside a composite literal
// — is in none of those lists, so storm refuses to run it. That refusal is the
// point: it is what stops a statement assembled from a caller's string, which
// is the one way a value can reach SQL text in this library.
//
// The refusal is correct either way. What this adds is WHEN: a build error
// naming the line, instead of an error on the request that first takes that
// branch.
type Undeclarable struct {
	// Pos is file:line, for the error.
	Pos string
	// Fn is the storm function called, as written.
	Fn string
	// Why is what the rule found, phrased for the developer.
	Why string
}

// scanUndeclarable finds every storm.SQL/storm.SQLExec call in the file that
// is not the value of a package-level var.
//
// By subtraction rather than by walking function bodies: a declaration can be
// nested in a composite literal or a closure that is itself a package-level
// var, and those are just as invisible to discovery as one inside a handler.
// Marking the legitimate ones and reporting the rest cannot miss a shape
// nobody thought of.
func scanUndeclarable(fset *token.FileSet, f *ast.File, scan *pkgScan, is func(ast.Expr, string) bool) {
	declared := map[ast.Expr]bool{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.VAR {
			continue
		}
		for _, spec := range g.Specs {
			s, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, v := range s.Values {
				if isStormCall(v, is) {
					declared[v] = true
				}
			}
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isStormCall(call, is) || declared[ast.Expr(call)] {
			return true
		}
		scan.undeclarable = append(scan.undeclarable, Undeclarable{
			Pos: fset.Position(call.Pos()).String(),
			Fn:  stormCallName(call, is),
			Why: "is not a package-level var, so it is not discovered, not PREPAREd " +
				"and not registered — storm will refuse to run it",
		})
		return true
	})

	scanRegistration(fset, f, scan, is)
}

// scanRegistration finds a RegisterStatement whose argument is assembled at
// run time.
//
// Registration is the generator's job, and generated files are not scanned at
// all. A hand-written call is legitimate — a test standing in for the
// generator does exactly this — but only with text fixed in the source. A call
// in it means whatever that expression produces gets whitelisted, which is the
// one thing the statement check exists to prevent: it would let an injected
// statement register itself and then run.
//
// The rule is a call expression anywhere in the argument, because that is what
// can be decided with certainty from the syntax alone. A bare identifier is
// left alone: telling a const from a var needs the whole package, and a var
// holding a fixed string is registered with the value it actually has, which
// the runtime check then pins.
func scanRegistration(fset *token.FileSet, f *ast.File, scan *pkgScan, is func(ast.Expr, string) bool) {
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !is(call.Fun, "RegisterStatement") || len(call.Args) != 1 {
			return true
		}
		if !containsCall(call.Args[0]) {
			return true
		}
		scan.undeclarable = append(scan.undeclarable, Undeclarable{
			Pos: fset.Position(call.Pos()).String(),
			Fn:  "storm.RegisterStatement",
			Why: "registers a statement built at run time, which whitelists whatever " +
				"that expression produces — the registry exists to stop exactly that",
		})
		return true
	})
}

// containsCall reports whether e contains a call expression, which is what
// separates text fixed in the source from text computed at run time.
func containsCall(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if _, ok := n.(*ast.CallExpr); ok {
			found = true
		}
		return !found
	})
	return found
}

// stormCallName reports which half of the escape hatch was called, for the
// error. The two have different fixes — one has a row type to move with it.
func stormCallName(call *ast.CallExpr, is func(ast.Expr, string) bool) string {
	if _, ok := call.Fun.(*ast.IndexExpr); ok {
		return "storm.SQL"
	}
	if is(call.Fun, "SQLExec") {
		return "storm.SQLExec"
	}
	return "storm.SQL"
}
