package codegen_test

import (
	"crypto/sha256"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gsoultan/raorm"
	"github.com/gsoultan/raorm/codegen"
	"github.com/gsoultan/raorm/internal/testmodel"
	"github.com/gsoultan/raorm/schema"
)

func fixtureSchema(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := raorm.Build(testmodel.All()...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A bounded context is generated in one call, not one table at a time. M3's
// relations and M4's FK-ordered flush both need to see the whole graph, and
// neither can be built on a generator that is handed one table name.
func TestPackage_EveryFixtureTable(t *testing.T) {
	s := fixtureSchema(t)
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir:    "gen",
		Import: "github.com/gsoultan/raorm",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(s.Tables) {
		t.Fatalf("got %d packages for %d tables", len(files), len(s.Tables))
	}
	for path, src := range files {
		// One package per table, in its own directory: gen/user/user.gen.go.
		dir, base := filepath.Split(path)
		pkg := strings.TrimSuffix(dir, "/")
		if base != pkg+".gen.go" {
			t.Errorf("%s: file should be %s.gen.go", path, pkg)
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.AllErrors)
		if err != nil {
			t.Errorf("%s: generated code does not parse: %v", path, err)
			continue
		}
		if f.Name.Name != pkg {
			t.Errorf("%s: package clause is %q, want %q", path, f.Name.Name, pkg)
		}
	}
}

// Determinism is a promise about the whole tree, not one file: `raorm verify`
// fails CI on stale output, so a map iteration leaking into the bytes would
// make it fail at random.
func TestPackage_Deterministic(t *testing.T) {
	s := fixtureSchema(t)
	var want [32]byte
	for i := range 10 {
		files, err := codegen.Package(s, codegen.PackageOptions{
			Dir: "gen", Import: "github.com/gsoultan/raorm",
		})
		if err != nil {
			t.Fatal(err)
		}
		paths := make([]string, 0, len(files))
		for p := range files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		h := sha256.New()
		for _, p := range paths {
			h.Write([]byte(p))
			h.Write(files[p])
		}
		var got [32]byte
		copy(got[:], h.Sum(nil))
		if i == 0 {
			want = got
		} else if got != want {
			t.Fatalf("run %d differs — generated output must be byte-deterministic", i)
		}
	}
}

// The package name comes from the Go type, because pluralisation is not
// invertible: "users" -> "user" is easy and "addresses" -> "addres" is wrong.
func TestPackageName_FromGoNameNotTableName(t *testing.T) {
	for _, tc := range []struct {
		goName, table, want string
	}{
		{"User", "users", "user"},
		{"Address", "addresses", "address"},
		{"OrgMember", "org_members", "orgmember"},
		{"", "legacy_rows", "legacyrows"}, // introspected: no Go type to consult
	} {
		got, err := codegen.PackageName(tc.goName, tc.table)
		if err != nil {
			t.Errorf("PackageName(%q, %q): %v", tc.goName, tc.table, err)
			continue
		}
		if got != tc.want {
			t.Errorf("PackageName(%q, %q) = %q, want %q", tc.goName, tc.table, got, tc.want)
		}
	}
}

func TestPackageName_Rejected(t *testing.T) {
	for _, tc := range []struct{ goName, table, why string }{
		{"Type", "types", "Go keyword"},
		{"", "123", "not an identifier"},
		{"", "_", "empty after mapping"},
	} {
		if _, err := codegen.PackageName(tc.goName, tc.table); err == nil {
			t.Errorf("PackageName(%q, %q) should fail: %s", tc.goName, tc.table, tc.why)
		}
	}
}

// Two tables landing in one directory would overwrite each other. Silence is
// the one outcome AGENTS.md does not allow.
func TestPackage_CollidingNamesAreAnError(t *testing.T) {
	s := fixtureSchema(t)
	if len(s.Tables) < 2 {
		t.Skip("need two tables")
	}
	clash := *s.Tables[1]
	clash.Name += "_alias"
	clash.GoName = s.Tables[0].GoName // same Go type name, different table
	s2 := *s
	s2.Tables = append(append([]*schema.Table{}, s.Tables...), &clash)

	_, err := codegen.Package(&s2, codegen.PackageOptions{
		Dir: "gen", Import: "github.com/gsoultan/raorm",
	})
	if err == nil {
		t.Fatal("two tables generating the same package must be an error")
	}
	if !strings.Contains(err.Error(), "both generate package") {
		t.Errorf("error should name the collision, got: %v", err)
	}
}

func TestPackage_RequiresImportPath(t *testing.T) {
	s := fixtureSchema(t)
	if _, err := codegen.Package(s, codegen.PackageOptions{Dir: "gen"}); err == nil {
		t.Fatal("generating without an import path must fail")
	}
}

// A mutual foreign-key reference has no write order that satisfies both, and
// the generator must say so rather than emitting an arbitrary one.
func TestFlushOrder_CycleIsAnError(t *testing.T) {
	s := fixtureSchema(t)
	a := &schema.Table{Name: "cyc_a", PrimaryKey: []string{"id"}}
	b := &schema.Table{Name: "cyc_b", PrimaryKey: []string{"id"}}
	a.ForeignKeys = []*schema.ForeignKey{{RefTable: "cyc_b"}}
	b.ForeignKeys = []*schema.ForeignKey{{RefTable: "cyc_a"}}

	s2 := *s
	s2.Tables = append(append([]*schema.Table{}, s.Tables...), a, b)

	_, err := codegen.FlushOrder(&s2)
	if err == nil {
		t.Fatal("a foreign-key cycle must be a generation error")
	}
	if !strings.Contains(err.Error(), "cyc_a") || !strings.Contains(err.Error(), "cyc_b") {
		t.Errorf("the error must name the cycle, got: %v", err)
	}
}

// A self-reference orders rows, not tables. Treating it as a cycle would make
// every hierarchy unwritable.
func TestFlushOrder_SelfReferenceIsNotACycle(t *testing.T) {
	s := fixtureSchema(t)
	rank, err := codegen.FlushOrder(s)
	if err != nil {
		t.Fatalf("the fixture has a self-referencing orgs table: %v", err)
	}
	if _, ok := rank["orgs"]; !ok {
		t.Error("orgs is missing from the flush order")
	}
}

// A plan's cost is knowable at generate time, and lint is what turns "a
// reviewer can read it off plans.go" into "CI checks it". The numbers are the
// worst case: empty levels skip their query at run time.
func TestPlanCosts(t *testing.T) {
	s := fixtureSchema(t)
	var names []string
	for _, tb := range s.Tables {
		names = append(names, tb.Name)
	}
	costs, err := codegen.PlanCosts(s, names)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		"UserFeed":    4, // users + posts + nested comments + org
		"UserSummary": 2, // users + org
		"OrgTree":     3, // orgs + children + users
	}
	got := map[string]int{}
	for _, c := range costs {
		got[c.Name] = c.RoundTrips
		if c.Chain == "" {
			t.Errorf("%s has no rendered chain", c.Name)
		}
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s costs %d round trips, want %d", name, got[name], n)
		}
	}
}
