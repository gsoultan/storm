package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

// A semi-join probe narrows the parent's query; it must not widen it. Every
// read of a soft-delete table leaves its marked rows out, and the probe's
// statement is built in the context package rather than the table's own, so
// it has to carry the parent's predicate itself — the child's is in the
// EXISTS header.

type hsParent struct {
	storm.Model
	Name      string
	DeletedAt *time.Time
	Children  []hsChild
}

func (p *hsParent) Schema(t *storm.Table) {
	t.Col(&p.Name).Size(40)
	t.SoftDelete(&p.DeletedAt)
}

type hsChild struct {
	storm.Model
	Label     string
	DeletedAt *time.Time
	Parent    hsParent
}

func (c *hsChild) Schema(t *storm.Table) {
	t.Col(&c.Label).Size(40)
	t.SoftDelete(&c.DeletedAt)
}

func TestASemiJoinLeavesTheParentsDeletedRowsOutAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&hsParent{}, &hsChild{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "hslive" + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })

	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: dir, Import: "github.com/gsoultan/storm",
		Package: "store", PackageImport: "github.com/gsoultan/storm/internal/" + base,
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel, src := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := strings.ReplaceAll(havingSoftDeleteLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a semi-join reads the parent's deleted rows:\n%s", out)
	}
	for _, name := range []string{
		"TestHavingLeavesADeletedParentOut",
		"TestNotHavingLeavesADeletedParentOut",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

const havingSoftDeleteLiveSrc = `package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"

	store "IMPORTPATH"
	"IMPORTPATH/hschild"
	"IMPORTPATH/hsparent"
)

type hsParent struct {
	storm.Model
	Name      string
	DeletedAt *time.Time
	Children  []hsChild
}

func (p *hsParent) Schema(t *storm.Table) {
	t.Col(&p.Name).Size(40)
	t.SoftDelete(&p.DeletedAt)
}

type hsChild struct {
	storm.Model
	Label     string
	DeletedAt *time.Time
	Parent    hsParent
}

func (c *hsChild) Schema(t *storm.Table) {
	t.Col(&c.Label).Size(40)
	t.SoftDelete(&c.DeletedAt)
}

var (
	pool *pgxpool.Pool
	ex   runtime.Executor
	// live has a live child; gone was deleted and has a live child; bare is
	// live and has only a deleted child.
	live = [16]byte{1, 15: 1}
	gone = [16]byte{2, 15: 2}
	bare = [16]byte{3, 15: 3}
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "STORM_DSN did not reach the generated package")
		os.Exit(1)
	}
	ctx := context.Background()
	s, err := storm.Build(&hsParent{}, &hsChild{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_having_softdelete_live"
	_, err = p.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE; CREATE SCHEMA "+ns)
	must(err)
	_, err = p.Exec(ctx, "SET search_path TO "+ns+"; "+pgddl.Create(s))
	must(err)
	cfg := p.Config()
	cfg.ConnConfig.RuntimeParams["search_path"] = ns
	p.Close()
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	ex = pgxdrv.Pool{P: pool}

	for i, id := range [][16]byte{live, gone, bare} {
		must(hsparent.Insert(ctx, ex, &hsparent.Row{ID: id, Name: fmt.Sprint(i)}))
		child := [16]byte{9, byte(i), 15: 9}
		must(hschild.Insert(ctx, ex, &hschild.Row{ID: child, Label: fmt.Sprint(i), ParentID: id}))
		if id == bare {
			must(hschild.Delete(ctx, ex, child))
		}
	}
	must(hsparent.Delete(ctx, ex, gone))

	code := m.Run()
	_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE")
	pool.Close()
	os.Exit(code)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func TestHavingLeavesADeletedParentOut(t *testing.T) {
	ctx := context.Background()
	n, err := store.HsParentHavingChildren(hsparent.New()).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d parents have a live child, want 1: the deleted parent was counted", n)
	}
	rows, err := store.HsParentHavingChildren(hsparent.New()).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != live {
		t.Errorf("the parents with a live child are %+v, want only the live one", rows)
	}
}

func TestNotHavingLeavesADeletedParentOut(t *testing.T) {
	ctx := context.Background()
	rows, err := store.HsParentNotHavingChildren(hsparent.New()).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != bare {
		t.Errorf("the parents with no live child are %+v, want only the one whose child was deleted", rows)
	}
	n, err := store.HsParentNotHavingChildren(hsparent.New()).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d parents have no live child, want 1", n)
	}
}
`
