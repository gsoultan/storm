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

// UpsertWhere: record a worker, rewriting its row only when the row there is
// stale or says another version — one statement, the guard read from the row
// the insert collided with.

type uwWorker struct {
	storm.Model
	Topic    string
	Worker   string
	Version  *string
	LastSeen time.Time
}

func (w *uwWorker) Schema(t *storm.Table) {
	t.Col(&w.Topic).Size(32)
	t.Col(&w.Worker).Size(32)
	t.Col(&w.Version).Size(32)
	t.Unique(&w.Topic, &w.Worker)
	t.GuardsUpsert()
}

func TestUpsertWhereAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&uwWorker{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "uwlive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(upsertWhereLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	src = strings.ReplaceAll(src, "MODELS", upsertWhereModels)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a guarded upsert does not write what it says:\n%s", out)
	}
	for _, name := range []string{
		"TestTheGuardDecidesTheUpdate",
		"TestUpsertWhereRefusesNoUpdate",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

const upsertWhereModels = `type uwWorker struct {
	storm.Model
	Topic    string
	Worker   string
	Version  *string
	LastSeen time.Time
}

func (w *uwWorker) Schema(t *storm.Table) {
	t.Col(&w.Topic).Size(32)
	t.Col(&w.Worker).Size(32)
	t.Col(&w.Version).Size(32)
	t.Unique(&w.Topic, &w.Worker)
	t.GuardsUpsert()
}
`

const upsertWhereLiveSrc = `package store_test

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

	"IMPORTPATH/uwworker"
)

MODELS

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
	s, err := storm.Build(&uwWorker{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_upsert_where_live"
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

// seen records the worker at, rewriting its row only when the row is older
// than stale or says another version.
func seen(t *testing.T, version string, at, stale time.Time) int64 {
	t.Helper()
	n := uwworker.Create()
	n.SetTopic("ship")
	n.SetWorker("w1")
	n.SetVersion(version)
	n.SetLastSeen(at)
	n.OnConflictTopicWorker()
	n2, err := n.UpsertWhere(context.Background(), ex, uwworker.New().
		Any(uwworker.LastSeen.Lt(stale), uwworker.Version.NotEq(version), uwworker.Version.IsNull()))
	if err != nil {
		t.Fatal(err)
	}
	return n2
}

func lastSeen(t *testing.T) (time.Time, string) {
	t.Helper()
	rows, err := uwworker.New().All(context.Background(), ex, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read %d rows (%v)", len(rows), err)
	}
	return rows[0].LastSeen, rows[0].Version.V
}

func TestTheGuardDecidesTheUpdate(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if n := seen(t, "1", base, base.Add(-10*time.Second)); n != 1 {
		t.Fatalf("the first sighting wrote %d rows", n)
	}
	// Five seconds on, same version: the row is fresh, so it stays.
	if n := seen(t, "1", base.Add(5*time.Second), base.Add(-5*time.Second)); n != 0 {
		t.Errorf("a fresh row was rewritten (%d)", n)
	}
	if at, _ := lastSeen(t); !at.Equal(base) {
		t.Errorf("last seen %v, want %v", at, base)
	}
	// Another version: rewritten at once.
	if n := seen(t, "2", base.Add(6*time.Second), base.Add(-4*time.Second)); n != 1 {
		t.Errorf("a new version was not written (%d)", n)
	}
	// Stale: rewritten.
	if n := seen(t, "2", base.Add(20*time.Second), base.Add(10*time.Second)); n != 1 {
		t.Errorf("a stale row was not written (%d)", n)
	}
	if at, v := lastSeen(t); !at.Equal(base.Add(20*time.Second)) || v != "2" {
		t.Errorf("row is %v %q", at, v)
	}
}

func TestUpsertWhereRefusesNoUpdate(t *testing.T) {
	n := uwworker.Create()
	n.SetTopic("x")
	if _, err := n.UpsertWhere(context.Background(), ex, uwworker.New()); err == nil {
		t.Error("an upsert with no key ran")
	}
	n.OnConflictTopicWorker().DoNothing()
	if _, err := n.UpsertWhere(context.Background(), ex, uwworker.New()); err == nil {
		t.Error("DoNothing ran")
	}
	n = uwworker.Create()
	n.SetTopic("x")
	n.OnConflictTopicWorker()
	if _, err := n.UpsertWhere(context.Background(), ex, uwworker.New().Limit(1)); err == nil {
		t.Error("a limited guard ran")
	}
}
`
