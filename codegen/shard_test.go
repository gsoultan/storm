package codegen_test

// A sharded model's generated calls must take a shard.Bound, because that is
// the whole compile-time half of sharding: a pool is not a Bound, so the query
// that does not say which tenant it is for does not build. These tests assert
// the emitted signature, the import that makes it compile, and that an
// UNSHARDED model in the same context is untouched — the last one because a
// change that made every table sharded would pass the first two.

import (
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
type Invoice struct {
	storm.Model

	TenantID storm.UUID
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
