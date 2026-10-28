package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

// LockTables and LockTimeout: a sweep that must not race the writers of two
// tables locks both for its transaction; a writer that would rather fail than
// wait bounds its wait.

type lkHold struct {
	storm.Model
	Note string
}

func (h *lkHold) Schema(t *storm.Table) { t.Col(&h.Note).Size(16); t.Lockable() }

type lkPeriod struct {
	storm.Model
	Days int
}

func (p *lkPeriod) Schema(t *storm.Table) { t.Lockable() }

type lkOther struct {
	storm.Model
}

func TestLockTablesAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&lkHold{}, &lkPeriod{}, &lkOther{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "lklive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(lockTablesLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	src = strings.ReplaceAll(src, "MODELS", lockTablesModels)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a table lock does not hold what it says:\n%s", out)
	}
	for _, name := range []string{
		"TestALockHoldsOffAWriterUntilCommit",
		"TestLockTablesRefusesNothingToLock",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

const lockTablesModels = `type lkHold struct {
	storm.Model
	Note string
}

func (h *lkHold) Schema(t *storm.Table) { t.Col(&h.Note).Size(16); t.Lockable() }

type lkPeriod struct {
	storm.Model
	Days int
}

func (p *lkPeriod) Schema(t *storm.Table) { t.Lockable() }

type lkOther struct {
	storm.Model
}
`

const lockTablesLiveSrc = `package store_test

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
	"IMPORTPATH/lkhold"
)

MODELS

var (
	pool *pgxpool.Pool
	db   pgxdrv.Pool
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "STORM_DSN did not reach the generated package")
		os.Exit(1)
	}
	ctx := context.Background()
	s, err := storm.Build(&lkHold{}, &lkPeriod{}, &lkOther{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_lock_tables_live"
	_, err = p.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE; CREATE SCHEMA "+ns)
	must(err)
	_, err = p.Exec(ctx, "SET search_path TO "+ns+"; "+pgddl.Create(s))
	must(err)
	cfg := p.Config()
	cfg.ConnConfig.RuntimeParams["search_path"] = ns
	p.Close()
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	db = pgxdrv.Pool{P: pool}

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

func insert(ctx context.Context, ex runtime.Executor) error {
	h := lkhold.Create()
	h.SetNote("x")
	_, err := h.Insert(ctx, ex)
	return err
}

// A SHARE lock on both tables holds a writer off until the lock's
// transaction ends; the writer's bounded wait fails rather than waits.
func TestALockHoldsOffAWriterUntilCommit(t *testing.T) {
	ctx := context.Background()
	sweep, err := db.StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sweep.Rollback(ctx)
	if err := store.LockTables(ctx, sweep, store.LockShare, store.TableLkHold, store.TableLkPeriod); err != nil {
		t.Fatal(err)
	}
	writer, err := db.StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	if err := store.LockTimeout(ctx, writer, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = insert(ctx, writer)
	if !errors.Is(err, runtime.ErrLockNotAvailable) {
		t.Fatalf("the writer got %v, want ErrLockNotAvailable", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("the writer waited %v past its bound", waited)
	}
	if err := sweep.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := insert(ctx, db); err != nil {
		t.Errorf("after the commit the write failed: %v", err)
	}
}

func TestLockTablesRefusesNothingToLock(t *testing.T) {
	ctx := context.Background()
	if err := store.LockTables(ctx, db, 0, store.TableLkHold); err == nil {
		t.Error("no mode locked")
	}
	if err := store.LockTables(ctx, db, store.LockShare); err == nil {
		t.Error("no table locked")
	}
}
`
