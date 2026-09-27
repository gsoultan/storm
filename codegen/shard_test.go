package codegen_test

// A sharded model's generated calls must take a shard.Bound, because that is
// the whole compile-time half of sharding: a pool is not a Bound, so the query
// that does not say which tenant it is for does not build. These tests assert
// the emitted signature, the import that makes it compile, and that an
// UNSHARDED model in the same context is untouched — the last one because a
// change that made every table sharded would pass the first two.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/schema"
)

// Invoice lives on the shard its tenant lives on.
//
// It carries a RELATION on purpose. Without one the context file emits no
// plan type, and the plan types were where the Bound requirement leaked: the
// context has no single driving table, so execType read nothing there and
// gave every plan a runtime.Executor — including plans over a sharded parent,
// which is a package that does not compile. A fixture with no relation could
// not see it, and did not.
type Invoice struct {
	storm.Model

	TenantID storm.UUID
	Currency *Currency
	Total    storm.Decimal
	Status   string
}

func (i *Invoice) Schema(t *storm.Table) {
	t.ShardKey(&i.TenantID)
}

// Currency is a reference table: unsharded, copied to every shard.
type Currency struct {
	storm.Model

	Code string
}

func (c *Currency) Schema(t *storm.Table) {
	t.Col(&c.Code).Unique().Size(3)
}

func shardedFixture(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := storm.Build(&Invoice{}, &Currency{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return s
}

func generate(t *testing.T, s *schema.Schema) map[string][]byte {
	t.Helper()
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir:    "gen",
		Import: "github.com/gsoultan/storm",
	})
	if err != nil {
		t.Fatalf("Package: %v", err)
	}
	return files
}

func sourceFor(t *testing.T, files map[string][]byte, pkg string) string {
	t.Helper()
	src, ok := files[pkg+"/"+pkg+".gen.go"]
	if !ok {
		keys := make([]string, 0, len(files))
		for k := range files {
			keys = append(keys, k)
		}
		t.Fatalf("no package %q in %v", pkg, keys)
	}
	return string(src)
}

func TestShardedModelTakesABound(t *testing.T) {
	src := sourceFor(t, generate(t, shardedFixture(t)), "invoice")

	if strings.Contains(src, "ex runtime.Executor") {
		t.Error("a sharded model emitted `ex runtime.Executor`; a pool would compile and " +
			"read one shard's rows as if they were all of them")
	}
	if !strings.Contains(src, "ex shard.Bound") {
		t.Fatal("a sharded model emitted no `ex shard.Bound`")
	}
	if !strings.Contains(src, `"github.com/gsoultan/storm/runtime/shard"`) {
		t.Error("the shard import is missing, so the package names a type it never imported")
	}
}

// The signature has to change on EVERY call, not on the read path only. A
// write aimed at the wrong shard is the expensive half: a read returns too few
// rows and a write puts a row somewhere it will never be found again.
func TestShardedModelTakesABoundOnWritesToo(t *testing.T) {
	src := sourceFor(t, generate(t, shardedFixture(t)), "invoice")

	for _, fn := range []string{
		"func (q Query) All(",
		"func (q Query) One(",
		"func (q Query) Count(",
		"func (q Query) Exists(",
		"func InsertAll(",
	} {
		i := strings.Index(src, fn)
		if i < 0 {
			t.Errorf("%s not generated", fn)
			continue
		}
		sig := src[i:]
		if end := strings.Index(sig, "\n"); end >= 0 {
			sig = sig[:end]
		}
		if !strings.Contains(sig, "shard.Bound") {
			t.Errorf("%s takes no Bound: %s", fn, sig)
		}
	}
}

// The unsharded neighbour in the same context keeps the plain port. Sharding
// one table must not conscript the rest of the schema.
func TestUnshardedNeighbourIsUnchanged(t *testing.T) {
	src := sourceFor(t, generate(t, shardedFixture(t)), "currency")

	if strings.Contains(src, "shard.Bound") {
		t.Error("an unsharded model was given a Bound")
	}
	if strings.Contains(src, "runtime/shard") {
		t.Error("an unsharded model imports the shard package for nothing")
	}
	if !strings.Contains(src, "ex runtime.Executor") {
		t.Error("an unsharded model lost the plain Executor")
	}
}

// The signature assertions above compare TEXT, and text that names a type is
// not the same as a package that builds: an import left out, a Bound used
// where the body wanted an Executor, or a plan type that threads the wrong one
// would all pass them. This builds the sharded context, which is the claim.
func TestShardedPackageCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the sharded fixture; -short skips it")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("internal", "shardgen"+strconv.Itoa(os.Getpid()))
	dir := filepath.Join(root, rel)
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := codegen.Package(shardedFixture(t), codegen.PackageOptions{
		Dir:           dir,
		Import:        "github.com/gsoultan/storm",
		Package:       "billing",
		PackageImport: "github.com/gsoultan/storm/" + filepath.ToSlash(rel),
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, src := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("go", "build", "./"+filepath.ToSlash(rel)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the sharded context does not compile: %v\n%s", err, out)
	}
	cmd = exec.Command("go", "vet", "./"+filepath.ToSlash(rel)+"/...")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the sharded context does not vet clean: %v\n%s", err, out)
	}
}

// A unit of work spans the whole context, and the ops it stages are plain
// BatchOps that carry no shard. So a context holding even one sharded table
// must hand back the checking Unit — otherwise the cross-shard refusal exists
// in runtime/shard and never fires in generated code, which is where the
// writes actually are.
func TestShardedContextHandsBackACheckingUnit(t *testing.T) {
	files, err := codegen.Package(shardedFixture(t), codegen.PackageOptions{
		Dir:           "gen",
		Import:        "github.com/gsoultan/storm",
		Package:       "billing",
		PackageImport: "github.com/gsoultan/storm/gen",
	})
	if err != nil {
		t.Fatal(err)
	}
	src := string(files["billing.gen.go"])
	if src == "" {
		t.Fatal("no context file generated")
	}

	if !strings.Contains(src, "func NewUnit() *shard.Unit") {
		t.Error("a sharded context hands back a runtime.Unit, which stages a sharded " +
			"write against whatever Flush was called with")
	}
	if !strings.Contains(src, `"github.com/gsoultan/storm/runtime/shard"`) {
		t.Error("the context names shard.Unit without importing the package")
	}
}

func TestUnshardedContextKeepsThePlainUnit(t *testing.T) {
	s, err := storm.Build(&Currency{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir:           "gen",
		Import:        "github.com/gsoultan/storm",
		Package:       "refdata",
		PackageImport: "github.com/gsoultan/storm/gen",
	})
	if err != nil {
		t.Fatal(err)
	}
	src := string(files["refdata.gen.go"])
	if !strings.Contains(src, "func NewUnit() *runtime.Unit") {
		t.Error("an unsharded context lost the plain Unit")
	}
	if strings.Contains(src, "runtime/shard") {
		t.Error("an unsharded context imports the shard package for nothing")
	}
}

// ShardKeyOf exists so the shard key's name is written once — in the model —
// rather than at every call site where a typo routes every row consistently
// to the wrong shard. The constructor has to follow the column's type.
type shSlugTenant struct {
	storm.Model
	Slug string
}

func (m *shSlugTenant) Schema(t *storm.Table) {
	t.Col(&m.Slug).Size(64)
	t.ShardKey(&m.Slug)
}

type shAcctTenant struct {
	storm.Model
	AcctNo int32
}

func (m *shAcctTenant) Schema(t *storm.Table) { t.ShardKey(&m.AcctNo) }

func TestShardKeyOfFollowsTheColumnType(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model any
		pkg   string
		want  string
	}{
		{"uuid", &Invoice{}, "invoice", "func ShardKeyOf(r Row) shard.Key { return shard.UUIDKey(r.TenantID) }"},
		{"text", &shSlugTenant{}, "shslugtenant", "func ShardKeyOf(r Row) shard.Key { return shard.StringKey(r.Slug) }"},
		{"int", &shAcctTenant{}, "shaccttenant", "func ShardKeyOf(r Row) shard.Key { return shard.Int64Key(int64(r.AcctNo)) }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := storm.Build(tc.model, &Currency{})
			if err != nil {
				t.Fatal(err)
			}
			// By package name. The model sits beside Currency, so there are
			// two .gen.go files, and picking "the one a map range sees last"
			// made this pass or fail with Go's randomised iteration order.
			src := sourceFor(t, generate(t, s), tc.pkg)
			if !strings.Contains(src, tc.want) {
				t.Errorf("want:\n  %s\nnot in the generated package", tc.want)
			}
		})
	}
}

// An unsharded model gets no ShardKeyOf, because there is no key to read.
func TestUnshardedModelHasNoShardKeyOf(t *testing.T) {
	src := sourceFor(t, generate(t, shardedFixture(t)), "currency")
	if strings.Contains(src, "ShardKeyOf") {
		t.Error("an unsharded model emitted ShardKeyOf")
	}
}

// The Bound requirement is a PARAMETER TYPE, so it should be independent of
// the dialect — and "should be" is why this exists. The shard import and the
// signature are emitted by the same code for every back end, and a dialect
// that quietly fell back to runtime.Executor would un-check every routed
// query on that target only, which is the hardest kind of gap to notice.
func TestEveryDialectEmitsTheBound(t *testing.T) {
	s, err := storm.Build(&Invoice{}, &Currency{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []codegen.Dialect{
		codegen.DialectPostgres,
		codegen.DialectMySQL,
		codegen.DialectMSSQL,
		codegen.DialectOracle,
	} {
		t.Run(fmt.Sprint(d), func(t *testing.T) {
			files, err := codegen.Package(s, codegen.PackageOptions{
				Dir:     "gen",
				Import:  "github.com/gsoultan/storm",
				Dialect: d,
			})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			src := string(files["invoice/invoice.gen.go"])
			if !strings.Contains(src, "ex shard.Bound") {
				t.Error("no shard.Bound in the generated package")
			}
			if strings.Contains(src, "ex runtime.Executor") {
				t.Error("a sharded model kept runtime.Executor on this dialect")
			}
			if !strings.Contains(src, `"github.com/gsoultan/storm/runtime/shard"`) {
				t.Error("shard.Bound named without importing the package")
			}
		})
	}
}

// A plan over a sharded parent takes the parent's Bound, and so does every
// member loader it threads that executor into.
func TestShardedPlanTakesTheParentsBound(t *testing.T) {
	files, err := codegen.Package(shardedFixture(t), codegen.PackageOptions{
		Dir:           "gen",
		Import:        "github.com/gsoultan/storm",
		Package:       "billing",
		PackageImport: "github.com/gsoultan/storm/gen",
	})
	if err != nil {
		t.Fatal(err)
	}
	src := string(files["billing.gen.go"])
	if src == "" {
		t.Fatal("no context file generated")
	}
	if !strings.Contains(src, "InvoiceWithCurrency") {
		t.Fatal("the fixture generated no plan type; this test cannot see the leak")
	}
	for _, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, "InvoiceWithCurrency") || !strings.Contains(line, "ctx context.Context, ex ") {
			continue
		}
		if !strings.Contains(line, "shard.Bound") {
			t.Errorf("a plan over a sharded parent takes a plain Executor:\n  %s", strings.TrimSpace(line))
		}
	}
}
