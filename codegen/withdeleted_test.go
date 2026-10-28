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

// WithDeleted: a read of a soft-delete table that sees the marked rows too,
// and the restore it makes possible — "bring back the entry removed last
// under this key" is one Take over such a read.

type wdEntry struct {
	storm.Model
	Key       string
	DeletedAt *time.Time
}

func (e *wdEntry) Schema(t *storm.Table) {
	t.Col(&e.Key).Size(32)
	t.SoftDelete(&e.DeletedAt)
	t.Takes()
	t.ReadsDeleted()
}

func (e *wdEntry) Projections(p *storm.Projections) { p.Named("Ref", &e.ID) }

func TestWithDeletedAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&wdEntry{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "wdlive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(withDeletedLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	if err := os.WriteFile(filepath.Join(dir, "wdentry", "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/wdentry/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("WithDeleted does not read what it says:\n%s", out)
	}
	for _, name := range []string{
		"TestTheMarkedRowsAreRead",
		"TestTakeRestoresTheLastRemoved",
		"TestOtherTerminalsRefuseIt",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

func TestReadsDeletedWantsASoftDeleteTable(t *testing.T) {
	if _, err := storm.Build(&wdPlain{}); err == nil || !strings.Contains(err.Error(), "ReadsDeleted") {
		t.Errorf("ReadsDeleted on a table that deletes for real built: %v", err)
	}
}

type wdPlain struct {
	storm.Model
	Key string
}

func (p *wdPlain) Schema(t *storm.Table) { t.ReadsDeleted() }

// Only a table that asks has it: a soft-delete table that does not keeps
// every read to the live rows, with no way for a call site to widen it.
func TestWithDeletedIsOnlyWhereTheModelSaysSo(t *testing.T) {
	s, err := storm.Build(&dpJob{}, &dpIncident{})
	if err != nil {
		t.Fatal(err)
	}
	files, err := codegen.Package(s, codegen.PackageOptions{
		Dir: t.TempDir(), Import: "github.com/gsoultan/storm",
		Package: "store", PackageImport: "example.com/store",
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel, src := range files {
		if strings.Contains(string(src), "WithDeleted()") {
			t.Errorf("%s has WithDeleted without the model asking", rel)
		}
	}
}

const withDeletedLiveSrc = `package wdentry_test

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

	"IMPORTPATH/wdentry"
)

type wdEntry struct {
	storm.Model
	Key       string
	DeletedAt *time.Time
}

func (e *wdEntry) Schema(t *storm.Table) {
	t.Col(&e.Key).Size(32)
	t.SoftDelete(&e.DeletedAt)
	t.Takes()
	t.ReadsDeleted()
}

func (e *wdEntry) Projections(p *storm.Projections) { p.Named("Ref", &e.ID) }

var (
	pool *pgxpool.Pool
	ex   runtime.Executor
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "STORM_DSN did not reach the generated package")
		os.Exit(1)
	}
	ctx := context.Background()
	s, err := storm.Build(&wdEntry{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_with_deleted_live"
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

// seed empties the table and writes one live entry under key "k" and two
// removed ones, the second removed a minute after the first.
func seed(t *testing.T) (live, first, last [16]byte) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM wd_entries"); err != nil {
		t.Fatal(err)
	}
	var ids [3][16]byte
	for i := range ids {
		c := wdentry.Create()
		c.SetKey("k")
		r, err := c.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = r.ID
	}
	at := time.Now().Add(-time.Hour).UTC()
	for i, id := range ids[1:] {
		if _, err := pool.Exec(ctx, "UPDATE wd_entries SET deleted_at = $1 WHERE id = $2", at.Add(time.Duration(i)*time.Minute), id); err != nil {
			t.Fatal(err)
		}
	}
	return ids[0], ids[1], ids[2]
}

func TestTheMarkedRowsAreRead(t *testing.T) {
	seed(t)
	ctx := context.Background()
	q := wdentry.New().Where(wdentry.Key.Eq("k"))
	live, err := q.All(ctx, ex, nil)
	if err != nil || len(live) != 1 {
		t.Fatalf("the live read found %d (%v), want 1", len(live), err)
	}
	all, err := q.WithDeleted().All(ctx, ex, nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("WithDeleted found %d (%v), want 3", len(all), err)
	}
	if n, err := q.WithDeleted().Count(ctx, ex); err != nil || n != 3 {
		t.Errorf("WithDeleted counted %d (%v), want 3", n, err)
	}
	gone := wdentry.New().Where(wdentry.Key.Eq("k"), wdentry.DeletedAt.IsNotNull())
	if ok, err := gone.Exists(ctx, ex); err != nil || ok {
		t.Errorf("the live read found a marked row: %v %v", ok, err)
	}
	if ok, err := gone.WithDeleted().Exists(ctx, ex); err != nil || !ok {
		t.Errorf("WithDeleted did not find a marked row: %v %v", ok, err)
	}
	if _, ok, err := gone.WithDeleted().One(ctx, ex); err != nil || !ok {
		t.Errorf("One under WithDeleted found nothing: %v", err)
	}
	if refs, err := q.WithDeleted().AllRef(ctx, ex); err != nil || len(refs) != 3 {
		t.Errorf("the projection under WithDeleted read %d (%v), want 3", len(refs), err)
	}
	if refs, err := q.AllRef(ctx, ex); err != nil || len(refs) != 1 {
		t.Errorf("the live projection read %d (%v), want 1", len(refs), err)
	}
}

func TestTakeRestoresTheLastRemoved(t *testing.T) {
	_, first, last := seed(t)
	ctx := context.Background()
	var m wdentry.Mut
	m.SetDeletedAtNull()
	q := wdentry.New().WithDeleted().
		Where(wdentry.Key.Eq("k"), wdentry.DeletedAt.IsNotNull()).
		Order(wdentry.DeletedAt.Desc()).Limit(1)
	got, err := q.Take(ctx, ex, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != last || got[0].DeletedAt.Valid {
		t.Fatalf("restored %+v, want the entry removed last, live", got)
	}
	live, err := wdentry.New().Where(wdentry.Key.Eq("k")).Count(ctx, ex)
	if err != nil || live != 2 {
		t.Errorf("%d live after the restore (%v), want 2", live, err)
	}
	// Without WithDeleted the same Take sees no marked row to restore.
	if got, err := wdentry.New().Where(wdentry.ID.Eq(first), wdentry.DeletedAt.IsNotNull()).Take(ctx, ex, m, nil); err != nil || len(got) != 0 {
		t.Errorf("a live-only Take restored %+v (%v)", got, err)
	}
}

func TestOtherTerminalsRefuseIt(t *testing.T) {
	seed(t)
	ctx := context.Background()
	q := wdentry.New().WithDeleted().Where(wdentry.Key.Eq("k"))
	var m wdentry.Mut
	m.SetKey("x")
	if _, err := q.UpdateAll(ctx, ex, m); err == nil {
		t.Error("UpdateAll ran under WithDeleted")
	}
	if _, err := q.DeleteAll(ctx, ex); err == nil {
		t.Error("DeleteAll ran under WithDeleted")
	}
	n, err := wdentry.New().WithDeleted().Where(wdentry.Key.Eq("x")).Count(ctx, ex)
	if err != nil || n != 0 {
		t.Errorf("%d rows were written by refused calls (%v)", n, err)
	}
}
`
