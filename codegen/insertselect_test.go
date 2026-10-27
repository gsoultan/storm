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

// An INSERT … SELECT writes one row per row a query of another table selects,
// with the reference filled from that row's key, and reads nothing into Go.

type isBatch struct {
	storm.Model
	Name string
}

func (b *isBatch) Schema(t *storm.Table) { t.Col(&b.Name).Size(40) }

type isSource struct {
	storm.Model
	Kind      string
	DeletedAt *time.Time
	Marks     []isMark
}

func (s *isSource) Schema(t *storm.Table) {
	t.Col(&s.Kind).Size(16)
	t.SoftDelete(&s.DeletedAt)
}

type isMark struct {
	storm.Model
	Open   bool
	Source isSource
}

type isItem struct {
	storm.Model
	Batch  isBatch
	Source isSource
	Status string
	Detail string
}

func (i *isItem) Schema(t *storm.Table) {
	t.Col(&i.Status).Size(16)
	t.InsertsPer(&i.Source)
}

func TestInsertSelectCopiesTheKeysAReadSelectsAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&isBatch{}, &isSource{}, &isMark{}, &isItem{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "islive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(insertSelectLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("INSERT … SELECT does not copy the keys the read selects:\n%s", out)
	}
	for _, name := range []string{
		"TestOneItemPerSelectedSource",
		"TestTheSourcesOrderAndLimitPickTheRows",
		"TestASemiJoinPicksTheSources",
		"TestALockedOrOffsetSourceIsRefused",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

const insertSelectLiveSrc = `package store_test

import (
	"context"
	"errors"
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
	"IMPORTPATH/isitem"
	"IMPORTPATH/ismark"
	"IMPORTPATH/issource"
)

type isBatch struct {
	storm.Model
	Name string
}

func (b *isBatch) Schema(t *storm.Table) { t.Col(&b.Name).Size(40) }

type isSource struct {
	storm.Model
	Kind      string
	DeletedAt *time.Time
	Marks     []isMark
}

func (s *isSource) Schema(t *storm.Table) {
	t.Col(&s.Kind).Size(16)
	t.SoftDelete(&s.DeletedAt)
}

type isMark struct {
	storm.Model
	Open   bool
	Source isSource
}

type isItem struct {
	storm.Model
	Batch  isBatch
	Source isSource
	Status string
	Detail string
}

func (i *isItem) Schema(t *storm.Table) {
	t.Col(&i.Status).Size(16)
	t.InsertsPer(&i.Source)
}

var (
	pool  *pgxpool.Pool
	ex    runtime.Executor
	batch = [16]byte{0xb, 15: 0xb}
	// five sources a second apart: 1 and 3 of kind "a" carry an open mark,
	// 4 is of kind "a" and deleted.
	src [5][16]byte
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "STORM_DSN did not reach the generated package")
		os.Exit(1)
	}
	ctx := context.Background()
	s, err := storm.Build(&isBatch{}, &isSource{}, &isMark{}, &isItem{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_insert_select_live"
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

	_, err = pool.Exec(ctx, "INSERT INTO is_batches (id, name) VALUES ($1, 'b')", batch)
	must(err)
	start := time.Now().Add(-time.Hour).UTC()
	kinds := []string{"b", "a", "b", "a", "a"}
	for i := range src {
		src[i] = [16]byte{byte(i + 1), 15: byte(i + 1)}
		_, err = pool.Exec(ctx, "INSERT INTO is_sources (id, kind, created_at, updated_at) VALUES ($1, $2, $3, $3)",
			src[i], kinds[i], start.Add(time.Duration(i)*time.Second))
		must(err)
	}
	for _, i := range []int{1, 3, 4} {
		must(ismark.Insert(ctx, ex, &ismark.Row{ID: [16]byte{0xe, byte(i), 15: 0xe}, Open: true, SourceID: src[i]}))
	}
	must(issource.Delete(ctx, ex, src[4]))

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

func pending() isitem.Ins {
	ins := isitem.Create()
	ins.SetBatchID(batch)
	ins.SetStatus("pending")
	ins.SetDetail("")
	return ins
}

// items empties the table after reading which sources it holds, in order.
func items(t *testing.T) [][16]byte {
	t.Helper()
	ctx := context.Background()
	rows, err := isitem.New().Where(isitem.BatchID.Eq(batch)).Order(isitem.SourceID.Asc()).Limit(100).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM is_items"); err != nil {
		t.Fatal(err)
	}
	out := make([][16]byte, len(rows))
	for i, r := range rows {
		if r.Status != "pending" || r.ID == ([16]byte{}) {
			t.Errorf("item %+v was not written as staged", r)
		}
		out[i] = r.SourceID
	}
	return out
}

func TestOneItemPerSelectedSource(t *testing.T) {
	ctx := context.Background()
	n, err := store.InsertIsItemPerSource(ctx, ex, pending(), issource.New().Where(issource.Kind.Eq("a")))
	if err != nil {
		t.Fatal(err)
	}
	got := items(t)
	if n != 2 || len(got) != 2 || got[0] != src[1] || got[1] != src[3] {
		t.Errorf("inserted %d: %v, want the two live sources of kind a", n, got)
	}
}

func TestTheSourcesOrderAndLimitPickTheRows(t *testing.T) {
	ctx := context.Background()
	q := issource.New().Order(issource.CreatedAt.Desc()).Limit(2)
	n, err := store.InsertIsItemPerSource(ctx, ex, pending(), q)
	if err != nil {
		t.Fatal(err)
	}
	got := items(t)
	if n != 2 || len(got) != 2 || got[0] != src[2] || got[1] != src[3] {
		t.Errorf("inserted %d: %v, want the two newest live sources", n, got)
	}
}

func TestASemiJoinPicksTheSources(t *testing.T) {
	ctx := context.Background()
	n, err := store.IsSourceHavingMarks(issource.New(), ismark.Open.Eq(true)).
		InsertIsItemPerSource(ctx, ex, pending())
	if err != nil {
		t.Fatal(err)
	}
	got := items(t)
	if n != 2 || len(got) != 2 || got[0] != src[1] || got[1] != src[3] {
		t.Errorf("inserted %d: %v, want the live sources with an open mark", n, got)
	}
}

func TestALockedOrOffsetSourceIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, q := range []issource.Query{issource.New().ForUpdate(), issource.New().Offset(1)} {
		if _, err := store.InsertIsItemPerSource(ctx, ex, pending(), q); !errors.Is(err, runtime.ErrPagedWrite) {
			t.Errorf("got %v, want ErrPagedWrite", err)
		}
	}
}
`
