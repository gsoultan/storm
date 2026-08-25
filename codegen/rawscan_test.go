package codegen_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gsoultan/raorm/codegen"
	"github.com/gsoultan/raorm/internal/aliasrow"
)

// A row-type package whose declared name differs from its directory (package
// aliasrowx in internal/aliasrow) must be imported under an alias matching
// the declared name, because scanner bodies qualify the type by that name.
// anubis hit this on day one: package authzrquery in a directory named
// rquery generated `rquery.AuthorizeRow` against a bare import that bound
// `authzrquery` — a generated file that did not compile.
func TestRawScanner_PackageNameNotDirectory(t *testing.T) {
	rt := reflect.TypeOf(aliasrowx.Row{})
	rs, err := codegen.ResolveRawScanner(rt, rt.PkgPath(), []codegen.RawField{
		{Name: "name", OID: 25}, // text
	})
	if err != nil {
		t.Fatal(err)
	}
	if rs.TypePkg != "aliasrowx" {
		t.Fatalf("TypePkg = %q, want the DECLARED package name %q", rs.TypePkg, "aliasrowx")
	}

	s := fixtureSchema(t)
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir:           "gen",
		Package:       "ctxgen",
		Import:        "github.com/gsoultan/raorm",
		PackageImport: "github.com/gsoultan/raorm/internal/testgen",
		RawScanners:   []codegen.RawScanner{rs},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ctx string
	for path, src := range files {
		if !strings.Contains(path, "/") {
			ctx = string(src)
		}
	}
	if ctx == "" {
		t.Fatal("no context package file emitted")
	}
	wantImport := `aliasrowx "github.com/gsoultan/raorm/internal/aliasrow"`
	if !strings.Contains(ctx, wantImport) {
		t.Fatalf("context file lacks aliased import %s", wantImport)
	}
	if !strings.Contains(ctx, "*aliasrowx.Row") {
		t.Fatal("scanner does not qualify the row type by the declared package name")
	}
	if strings.Contains(ctx, "aliasrow.Row") && !strings.Contains(ctx, "aliasrowx.Row") {
		t.Fatal("scanner qualifies by directory name, which does not compile")
	}
}
