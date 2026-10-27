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

// A step is a write (or a read) ahead of another write in the same statement,
// whose rows that write counts: an item settled and its batch's count moved
// on, in one round trip, with no moment where one is visible without the
// other.

type stBatch struct {
	storm.Model
	Status       string
	Skipped      int64
	Succeeded    int64
	FinishedAt   *time.Time
	LeaseExpires *time.Time
}

func (b *stBatch) Schema(t *storm.Table) {
	t.Col(&b.Status).Size(16)
	t.Tallies()
}

type stItem struct {
	storm.Model
	Batch  stBatch
	Status string
}

func (i *stItem) Schema(t *storm.Table) { t.Col(&i.Status).Size(16) }

func TestAWriteAfterAStepCountsItsRowsAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&stBatch{}, &stItem{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "stlive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(stepLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a write after a step does not count its rows:\n%s", out)
	}
	for _, name := range []string{
		"TestStopSkipsThePendingItemsAndCountsThem",
		"TestSettleMovesOneCountOnlyWhenItSettled",
		"TestFinishCompletesOnlyWhenNothingIsPending",
		"TestTheStepsArgumentsComeFirst",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

const stepLiveSrc = `package store_test

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

	"IMPORTPATH/stbatch"
	"IMPORTPATH/stitem"
)

type stBatch struct {
	storm.Model
	Status       string
	Skipped      int64
	Succeeded    int64
	FinishedAt   *time.Time
	LeaseExpires *time.Time
}

func (b *stBatch) Schema(t *storm.Table) {
	t.Col(&b.Status).Size(16)
	t.Tallies()
}

type stItem struct {
	storm.Model
	Batch  stBatch
	Status string
}

func (i *stItem) Schema(t *storm.Table) { t.Col(&i.Status).Size(16) }

var (
	pool  *pgxpool.Pool
	ex    runtime.Executor
	batch = [16]byte{0xb, 15: 0xb}
	other = [16]byte{0xc, 15: 0xc}
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "STORM_DSN did not reach the generated package")
		os.Exit(1)
	}
	ctx := context.Background()
	s, err := storm.Build(&stBatch{}, &stItem{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_step_live"
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

// seed writes a running batch with three pending items and one succeeded,
// and another batch with one pending item.
func seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM st_items; DELETE FROM st_batches"); err != nil {
		t.Fatal(err)
	}
	lease := time.Now().Add(time.Minute).UTC()
	for _, id := range [][16]byte{batch, other} {
		if err := stbatch.Insert(ctx, ex, &stbatch.Row{ID: id, Status: "running", LeaseExpires: runtime.Null[time.Time]{V: lease, Valid: true}}); err != nil {
			t.Fatal(err)
		}
	}
	for i, status := range []string{"pending", "pending", "pending", "succeeded"} {
		if err := stitem.Insert(ctx, ex, &stitem.Row{ID: item(i), BatchID: batch, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stitem.Insert(ctx, ex, &stitem.Row{ID: [16]byte{0xc, 15: 9}, BatchID: other, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
}

func item(i int) [16]byte { return [16]byte{0xa, byte(i), 15: 0xa} }

func read(t *testing.T, id [16]byte) stbatch.Row {
	t.Helper()
	row, found, err := stbatch.New().Where(stbatch.ID.Eq(id)).One(context.Background(), ex)
	if err != nil || !found {
		t.Fatalf("read batch: %v %v", found, err)
	}
	return row
}

func count(t *testing.T, batchID [16]byte, status string) int64 {
	t.Helper()
	n, err := stitem.New().Where(stitem.BatchID.Eq(batchID), stitem.Status.Eq(status)).Count(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStopSkipsThePendingItemsAndCountsThem(t *testing.T) {
	seed(t)
	ctx := context.Background()
	var skip stitem.Mut
	skip.SetStatus("skipped")
	var stop stbatch.Mut
	stop.SetStatus("stopped")
	stop.SetLeaseExpiresNull()
	var tally stbatch.Tally
	tally.AddSkipped()
	n, err := stbatch.New().Where(stbatch.ID.Eq(batch), stbatch.Status.Eq("running")).
		UpdateAllAfter(ctx, ex, stitem.New().Where(stitem.BatchID.Eq(batch), stitem.Status.Eq("pending")).UpdateAllStep(skip), stop, tally)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("stopped %d batches, want 1", n)
	}
	got := read(t, batch)
	if got.Status != "stopped" || got.Skipped != 3 || got.LeaseExpires.Valid {
		t.Errorf("the batch is %+v, want stopped with 3 skipped and no lease", got)
	}
	if count(t, batch, "skipped") != 3 || count(t, batch, "succeeded") != 1 || count(t, other, "pending") != 1 {
		t.Error("the step wrote other items than the batch's pending ones")
	}
}

// settle records one item's result and counts it, only over a pending item.
func settle(t *testing.T, i int) int64 {
	t.Helper()
	var done stitem.Mut
	done.SetStatus("succeeded")
	var touch stbatch.Mut
	touch.SetUpdatedAtNow()
	var tally stbatch.Tally
	tally.AddSucceeded()
	tally.OnlyIfAny()
	n, err := stbatch.New().Where(stbatch.ID.Eq(batch)).UpdateAllAfter(context.Background(), ex,
		stitem.New().Where(stitem.ID.Eq(item(i)), stitem.Status.Eq("pending")).UpdateAllStep(done), touch, tally)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSettleMovesOneCountOnlyWhenItSettled(t *testing.T) {
	seed(t)
	before := read(t, batch)
	if n := settle(t, 0); n != 1 {
		t.Errorf("settling a pending item wrote %d batches, want 1", n)
	}
	if n := settle(t, 0); n != 0 {
		t.Errorf("settling it again wrote %d batches, want 0", n)
	}
	got := read(t, batch)
	if got.Succeeded != 1 {
		t.Errorf("succeeded is %d, want 1", got.Succeeded)
	}
	if !got.UpdatedAt.After(before.UpdatedAt) && !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Error("updated_at moved backwards")
	}
}

func finish(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	var release stbatch.Mut
	release.SetLeaseExpiresNull()
	var tally stbatch.Tally
	tally.SetStatusIfNone("completed")
	tally.SetFinishedAtIfNone(now)
	if _, err := stbatch.New().Where(stbatch.ID.Eq(batch), stbatch.Status.Eq("running")).UpdateAllAfter(ctx, ex,
		stitem.New().Where(stitem.BatchID.Eq(batch), stitem.Status.Eq("pending")).ExistsStep(), release, tally); err != nil {
		t.Fatal(err)
	}
}

func TestFinishCompletesOnlyWhenNothingIsPending(t *testing.T) {
	seed(t)
	finish(t)
	got := read(t, batch)
	if got.Status != "running" || got.FinishedAt.Valid || got.LeaseExpires.Valid {
		t.Errorf("with items pending the batch is %+v, want running, unfinished, released", got)
	}
	for i := 0; i < 3; i++ {
		settle(t, i)
	}
	finish(t)
	got = read(t, batch)
	if got.Status != "completed" || !got.FinishedAt.Valid || got.Succeeded != 3 {
		t.Errorf("with nothing pending the batch is %+v, want completed and finished", got)
	}
}

// The step's placeholders come first and the write's number on after them,
// whatever each side binds.
func TestTheStepsArgumentsComeFirst(t *testing.T) {
	seed(t)
	ctx := context.Background()
	var skip stitem.Mut
	skip.SetStatus("skipped")
	var m stbatch.Mut
	m.SetStatus("checked")
	m.AddSucceeded(10)
	var tally stbatch.Tally
	tally.SubtractSkipped()
	tally.SetFinishedAtIfNone(time.Now().UTC())
	n, err := stbatch.New().Where(stbatch.ID.In(batch, other), stbatch.Status.Eq("running")).UpdateAllAfter(ctx, ex,
		stitem.New().Where(stitem.BatchID.Eq(other), stitem.Status.In("pending", "x")).UpdateAllStep(skip), m, tally)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("wrote %d batches, want 2", n)
	}
	for _, id := range [][16]byte{batch, other} {
		got := read(t, id)
		if got.Status != "checked" || got.Succeeded != 10 || got.Skipped != -1 || got.FinishedAt.Valid {
			t.Errorf("batch %v is %+v", id, got)
		}
	}
}
`
