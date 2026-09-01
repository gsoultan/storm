package tooldiscover

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// stormPath is the module whose types mark a struct as a model. Discovery is
// syntactic, so this is matched against import specs rather than resolved.
const stormPath = "github.com/gsoultan/storm"

// pkgScan accumulates one directory's evidence before any of it is judged.
//
// The rules are per-declaration but the verdict is per-type: a struct in one
// file with its Schema method in another is entirely ordinary, so nothing can
// be decided until the whole directory is parsed.
type pkgScan struct {
	name        string // package clause
	structs     map[string]token.Position
	reasons     map[string]Reason
	ignored     map[string]bool
	queries     map[string]token.Position
	unexported  map[string]token.Position // types that matched a rule but cannot be imported
	unexportedQ map[string]token.Position // ditto, raw query vars

	// unparsed are files that did not parse. Their models are invisible, so
	// reporting "no models found" would name the wrong cause.
	unparsed []string

	// Types embedded anonymously in some other struct. A MIXIN — a struct of
	// shared columns with its own Schema method, embedded into real models —
	// matches every rule a model matches and is not one. Being embedded is
	// what tells them apart, and mixins are routinely exported
	// (internal/testmodel has Auditable and SoftDelete), so nothing about the
	// declaration itself distinguishes them.
	localEmbeds map[string]bool // same-package: bare type name
	qualEmbeds  map[string]bool // other packages: "importpath.Type"
}

func newPkgScan() *pkgScan {
	return &pkgScan{
		structs:     map[string]token.Position{},
		reasons:     map[string]Reason{},
		ignored:     map[string]bool{},
		queries:     map[string]token.Position{},
		unexported:  map[string]token.Position{},
		unexportedQ: map[string]token.Position{},
		localEmbeds: map[string]bool{},
		qualEmbeds:  map[string]bool{},
	}
}

// rank orders the rules by how deliberate they are. A type can match several —
// Team embeds storm.Model AND has a Schema method — and `storm models` should
// report the one its author would give, not whichever file parsed last.
func rank(r Reason) int {
	switch r {
	case Directive:
		return 3 // the author said so outright
	case EmbedsModel:
		return 2 // visible in the declaration
	default:
		return 1 // inferred from a method
	}
}

// mark records a reason, keeping the most deliberate one seen.
func (p *pkgScan) mark(name string, r Reason, pos token.Position) {
	if cur, ok := p.reasons[name]; !ok || rank(r) > rank(cur) {
		p.reasons[name] = r
	}
	if _, ok := p.structs[name]; !ok {
		p.structs[name] = pos
	}
}

// scanDir parses one directory and returns its evidence. A directory with no
// storm import at all costs one parse per file and nothing else.
func scanDir(fset *token.FileSet, dir string) (*pkgScan, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		return nil, nil
	}
	sort.Strings(names)

	scan := newPkgScan()
	found := false
	for _, n := range names {
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			// Not this tool's error to explain — the compiler says it better.
			// But it IS this tool's job to say that the file was skipped:
			// otherwise a typo in the only model file reports "no models
			// found", which sends the developer looking for the wrong thing.
			scan.unparsed = append(scan.unparsed, err.Error())
			found = true
			continue
		}
		if isGenerated(f) {
			continue
		}
		if scan.name == "" {
			scan.name = f.Name.Name
		}
		found = true
		scanFile(fset, f, scan)
	}
	if !found {
		return nil, nil
	}
	return scan, nil
}

func scanFile(fset *token.FileSet, f *ast.File, scan *pkgScan) {
	// No early return for a file that does not import storm. Its structs still
	// have to be scanned for EMBEDS: a mixin is identified by being embedded,
	// and the struct doing the embedding may live in a file that names no
	// storm type at all. Skipping those files would let a mixin be reported as
	// a model and generate a table for it.
	local, dot := stormNames(f)

	// is reports whether an expression names the given storm type.
	is := func(e ast.Expr, want string) bool {
		switch t := e.(type) {
		case *ast.SelectorExpr:
			id, ok := t.X.(*ast.Ident)
			return ok && local[id.Name] && t.Sel.Name == want
		case *ast.Ident:
			return dot && t.Name == want
		}
		return false
	}
	// isPtr reports whether an expression is *storm.<want>.
	isPtr := func(e ast.Expr, want string) bool {
		s, ok := e.(*ast.StarExpr)
		return ok && is(s.X, want)
	}

	imports := importMap(f)
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			scanGenDecl(fset, d, scan, is, imports)
		case *ast.FuncDecl:
			scanMethod(fset, d, scan, isPtr)
		}
	}
}

// importMap maps each import's local name to its path, so an embedded
// `shared.Base` can be recognised as the same type `shared` declares.
func importMap(f *ast.File) map[string]string {
	m := make(map[string]string, len(f.Imports))
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		switch {
		case imp.Name == nil:
			m[path[strings.LastIndex(path, "/")+1:]] = path
		case imp.Name.Name == "." || imp.Name.Name == "_":
		default:
			m[imp.Name.Name] = path
		}
	}
	return m
}

// noteEmbed records an anonymous field's type. Relations are named fields
// (`Author Author`, `Articles []Article`), so nothing here can mistake one for
// a mixin.
func noteEmbed(e ast.Expr, scan *pkgScan, imports map[string]string) {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X // embedded *Base
	}
	switch t := e.(type) {
	case *ast.Ident:
		scan.localEmbeds[t.Name] = true
	case *ast.SelectorExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			if path, ok := imports[id.Name]; ok {
				scan.qualEmbeds[path+"."+t.Sel.Name] = true
			}
		}
	}
}

func scanGenDecl(fset *token.FileSet, d *ast.GenDecl, scan *pkgScan, is func(ast.Expr, string) bool, imports map[string]string) {
	for _, s := range d.Specs {
		switch s := s.(type) {
		case *ast.TypeSpec:
			st, ok := s.Type.(*ast.StructType)
			if !ok {
				continue
			}
			pos := fset.Position(s.Pos())
			// A directive may sit on the spec or, for a single-spec
			// declaration, on the `type` keyword above it.
			doc := s.Doc
			if doc == nil && len(d.Specs) == 1 {
				doc = d.Doc
			}
			ignore := hasDirective(doc, "ignore")
			if ignore {
				scan.ignored[s.Name.Name] = true
			} else if hasDirective(doc, "model") {
				record(scan, s.Name.Name, Directive, pos)
			}
			if st.Fields == nil {
				continue
			}
			for _, fld := range st.Fields.List {
				if len(fld.Names) != 0 {
					continue
				}
				// Recorded even for an ignored type: what IT embeds is still a
				// mixin, and ignoring the outer type must not resurrect the
				// inner one as a model.
				noteEmbed(fld.Type, scan, imports)
				if !ignore && is(fld.Type, "Model") {
					record(scan, s.Name.Name, EmbedsModel, pos)
				}
			}
		case *ast.ValueSpec:
			if d.Tok != token.VAR {
				continue
			}
			scanQueryVar(fset, s, scan, is)
		}
	}
}

// scanQueryVar finds `var X = storm.SQL[Row](...)` and `storm.SQLExec(...)`,
// in both the call form and the explicitly-typed form.
func scanQueryVar(fset *token.FileSet, s *ast.ValueSpec, scan *pkgScan, is func(ast.Expr, string) bool) {
	typed := isPtrTo(s.Type, is, "SQLQuery") || isPtrTo(s.Type, is, "SQLStmt")
	for i, name := range s.Names {
		hit := typed
		if !hit && i < len(s.Values) {
			hit = isStormCall(s.Values[i], is)
		}
		if !hit {
			continue
		}
		pos := fset.Position(name.Pos())
		if !ast.IsExported(name.Name) {
			scan.unexportedQ[name.Name] = pos
			continue
		}
		scan.queries[name.Name] = pos
	}
}

func isStormCall(e ast.Expr, is func(ast.Expr, string) bool) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := call.Fun.(type) {
	case *ast.IndexExpr: // storm.SQL[Row](...)
		return is(fn.X, "SQL")
	case *ast.SelectorExpr, *ast.Ident: // storm.SQLExec(...)
		return is(fn, "SQLExec") || is(fn, "SQL")
	}
	return false
}

func isPtrTo(e ast.Expr, is func(ast.Expr, string) bool, want string) bool {
	if e == nil {
		return false
	}
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	if idx, ok := star.X.(*ast.IndexExpr); ok { // *storm.SQLQuery[Row]
		return is(idx.X, want)
	}
	return is(star.X, want)
}

// scanMethod catches the model that embeds nothing. storm.Model is optional —
// a natural key is declared with t.PrimaryKey(...) inside Schema — so a rule
// that only looked for the embed would silently skip those models and generate
// a store with a table missing from it.
func scanMethod(fset *token.FileSet, d *ast.FuncDecl, scan *pkgScan, isPtr func(ast.Expr, string) bool) {
	if d.Recv == nil || len(d.Recv.List) != 1 || d.Type.Params == nil || len(d.Type.Params.List) != 1 {
		return
	}
	var reason Reason
	switch d.Name.Name {
	case "Schema":
		reason = HasSchema
	case "Plans":
		reason = HasPlans
	case "Projections":
		reason = HasProjections
	default:
		return
	}
	// The parameter type is what separates a storm declaration from any other
	// method that happens to be called Schema.
	want := map[Reason]string{HasSchema: "Table", HasPlans: "Plans", HasProjections: "Projections"}[reason]
	if !isPtr(d.Type.Params.List[0].Type, want) {
		return
	}
	name := recvTypeName(d.Recv.List[0].Type)
	if name == "" {
		return
	}
	record(scan, name, reason, fset.Position(d.Pos()))
}

// record applies a rule, respecting an ignore directive wherever it appeared.
func record(scan *pkgScan, name string, r Reason, pos token.Position) {
	if scan.ignored[name] {
		return
	}
	if !ast.IsExported(name) {
		scan.unexported[name] = pos
		return
	}
	scan.mark(name, r, pos)
}

func recvTypeName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// stormNames returns the local names storm is imported under in this file, and
// whether it was dot-imported. Aliases are ordinary in other people's code
// even though storm's own rules forbid them, so matching the literal
// identifier `storm` would quietly miss every aliased file.
func stormNames(f *ast.File) (map[string]bool, bool) {
	names, dot := map[string]bool{}, false
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != stormPath {
			continue
		}
		switch {
		case imp.Name == nil:
			names["storm"] = true
		case imp.Name.Name == ".":
			dot = true
		case imp.Name.Name == "_":
		default:
			names[imp.Name.Name] = true
		}
	}
	return names, dot
}

// hasDirective looks for //storm:<name> on its own line.
func hasDirective(doc *ast.CommentGroup, name string) bool {
	if doc == nil {
		return false
	}
	want := "//storm:" + name
	for _, c := range doc.List {
		if strings.TrimSpace(c.Text) == want {
			return true
		}
	}
	return false
}

// isGenerated applies the convention from go/build: a generated file says so
// on a line by itself before its package clause. storm's own output carries
// it, so without this check a second `generate` would discover the store it
// just wrote.
func isGenerated(f *ast.File) bool {
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		for _, c := range cg.List {
			t := strings.TrimSpace(c.Text)
			if strings.HasPrefix(t, "// Code generated ") && strings.HasSuffix(t, " DO NOT EDIT.") {
				return true
			}
		}
	}
	return false
}
