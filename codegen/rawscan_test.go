package codegen_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/internal/aliasrow"
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
		{Name: "note", OID: 25},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Nullable("Note") {
		t.Fatal("Null[string] field not recognized as nullable — generic Name() is Null[string], not Null")
	}
	if rs.TypePkg != "aliasrowx" {
		t.Fatalf("TypePkg = %q, want the DECLARED package name %q", rs.TypePkg, "aliasrowx")
	}

	s := fixtureSchema(t)
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir:           "gen",
		Package:       "ctxgen",
		Import:        "github.com/gsoultan/storm",
		PackageImport: "github.com/gsoultan/storm/internal/testgen",
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
	wantImport := `aliasrowx "github.com/gsoultan/storm/internal/aliasrow"`
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

// Two queries may share a row type — RegisterScanner keys by type, so the
// emitter must collapse them to ONE scanner — but only when their descriptors
// agree column for column. Scanners decode by position; a shared type over
// two column orders would transpose one query's values.
func TestRawScanner_SharedRowType(t *testing.T) {
	rt := reflect.TypeOf(aliasrowx.Row{})
	fields := []codegen.RawField{{Name: "name", OID: 25}, {Name: "note", OID: 25}}
	a, err := codegen.ResolveRawScanner(rt, rt.PkgPath(), fields)
	if err != nil {
		t.Fatal(err)
	}
	b, err := codegen.ResolveRawScanner(rt, rt.PkgPath(), fields)
	if err != nil {
		t.Fatal(err)
	}

	s := fixtureSchema(t)
	opts := codegen.PackageOptions{
		Dir:           "gen",
		Package:       "ctxgen",
		Import:        "github.com/gsoultan/storm",
		PackageImport: "github.com/gsoultan/storm/internal/testgen",
		RawScanners:   []codegen.RawScanner{a, b},
	}
	files, err := codegen.Package(s, opts)
	if err != nil {
		t.Fatalf("same shape must dedupe, not error: %v", err)
	}
	for path, src := range files {
		if !strings.Contains(path, "/") {
			if n := strings.Count(string(src), "func scanRow("); n != 1 {
				t.Fatalf("emitted %d scanners for one row type, want exactly 1", n)
			}
		}
	}

	// Same type, transposed columns: generation must refuse.
	swapped, err := codegen.ResolveRawScanner(rt, rt.PkgPath(),
		[]codegen.RawField{{Name: "note", OID: 25}, {Name: "name", OID: 25}})
	if err != nil {
		t.Fatal(err)
	}
	opts.RawScanners = []codegen.RawScanner{a, swapped}
	if _, err := codegen.Package(s, opts); err == nil {
		t.Fatal("transposed shared row type must fail generation")
	} else if !strings.Contains(err.Error(), "disagree at position 1") {
		t.Fatalf("error should name the position, got: %v", err)
	}
}

// sampleBucket spells its acronyms the way Go does. A model maps CPUPercent to
// cpu_percent, so a raw query's cpu_percent must land in it too.
type sampleBucket struct {
	Bucket     int64
	CPUPercent float64
	HTTPStatus int32
}

// The raw resolver used to know only GoName's direction — cpu_percent →
// CpuPercent — so a row type spelled the way the model side spells it was
// refused with a demand to rename the field. Root cause: two name mappings,
// one per side; both now go through schema.ColumnName.
func TestRawScanner_AcronymFieldMatchesModelMapping(t *testing.T) {
	rt := reflect.TypeOf(sampleBucket{})
	rs, err := codegen.ResolveRawScanner(rt, rt.PkgPath(), []codegen.RawField{
		{Name: "bucket", OID: 20},       // int8
		{Name: "cpu_percent", OID: 701}, // float8
		{Name: "http_status", OID: 23},  // int4
	})
	if err != nil {
		t.Fatalf("an acronym field the model side accepts was refused: %v", err)
	}
	s := fixtureSchema(t)
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: "gen", Package: "ctxgen", Import: "github.com/gsoultan/storm",
		PackageImport: "github.com/gsoultan/storm/internal/testgen",
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
	for _, want := range []string{".CPUPercent", ".HTTPStatus"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("scanner does not assign %s — it must use the struct's own field name", want)
		}
	}
	if strings.Contains(ctx, ".CpuPercent") {
		t.Error("scanner assigns CpuPercent, a field sampleBucket does not have")
	}
}

type ambiguousBucket struct {
	CpuPercent float64
	CPUPercent float64
}

type unfedBucket struct {
	Bucket     int64
	CPUPercent float64
}

// The suggestions have to stay true. A column with no field still names the
// GoName spelling, which works; an unfed field names the column the MODEL
// would give it, cpu_percent — not c_p_u_percent, which the old per-letter
// rule produced and which no alias could ever have satisfied.
func TestRawScanner_AcronymSuggestionsAreAccurate(t *testing.T) {
	rt := reflect.TypeOf(unfedBucket{})
	_, err := codegen.ResolveRawScanner(rt, rt.PkgPath(), []codegen.RawField{{Name: "bucket", OID: 20}})
	if err == nil || !strings.Contains(err.Error(), `aliased "cpu_percent"`) {
		t.Fatalf("unfed CPUPercent should suggest the alias cpu_percent: %v", err)
	}
	_, err = codegen.ResolveRawScanner(rt, rt.PkgPath(), []codegen.RawField{
		{Name: "bucket", OID: 20}, {Name: "cpu_percent", OID: 701}, {Name: "mem_mb", OID: 701},
	})
	if err == nil || !strings.Contains(err.Error(), "add `MemMb float64`") {
		t.Fatalf("a column with no field should suggest its GoName spelling: %v", err)
	}

	// The GoName spelling and the acronym spelling are both cpu_percent. One
	// of them would silently read as zero, so neither is picked... except
	// that GoName's exact field has always won, and still does: the existing
	// meaning of a row type does not change.
	rt = reflect.TypeOf(ambiguousBucket{})
	rs, err := codegen.ResolveRawScanner(rt, rt.PkgPath(), []codegen.RawField{{Name: "cpu_percent", OID: 701}})
	if err == nil {
		t.Fatalf("CPUPercent is fed by nothing and must be reported, got %+v", rs)
	}
	if !strings.Contains(err.Error(), "CPUPercent is fed by no result column") {
		t.Fatalf("unexpected error: %v", err)
	}
}
