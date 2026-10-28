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

// The bounded purge over a semi-join: the parent rows the probes select, at
// most the Limit, in one statement. "Forget the failed jobs no open incident
// names" is one of these.

type dpJob struct {
	storm.Model
	Status    string
	DeletedAt *time.Time
	Incidents []dpIncident
}

func (j *dpJob) Schema(t *storm.Table) {
	t.Col(&j.Status).Size(16)
	t.SoftDelete(&j.DeletedAt)
	t.Takes()
}

type dpIncident struct {
	storm.Model
	Job    *dpJob
	Status string
}

func (i *dpIncident) Schema(t *storm.Table) {
	t.Col(&i.Status).Size(16)
	t.Col(&i.Job).OnDelete(storm.SetNull)
}

func TestDeleteBatchOverASemiJoinAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&dpJob{}, &dpIncident{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "dplive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(deleteBatchProbeLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("DeleteBatch over a semi-join does not remove the rows it selects:\n%s", out)
	}
	for _, name := range []string{
		"TestTheProbePicksWhatIsPurged",
		"TestTheLimitBoundsABatch",
		"TestALockedOrOffsetProbeIsRefused",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

const deleteBatchProbeLiveSrc = `package store_test

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
	"IMPORTPATH/dpincident"
	"IMPORTPATH/dpjob"
)

type dpJob struct {
	storm.Model
	Status    string
	DeletedAt *time.Time
	Incidents []dpIncident
}

func (j *dpJob) Schema(t *storm.Table) {
	t.Col(&j.Status).Size(16)
	t.SoftDelete(&j.DeletedAt)
	t.Takes()
}

type dpIncident struct {
	storm.Model
	Job    *dpJob
	Status string
}

func (i *dpIncident) Schema(t *storm.Table) {
	t.Col(&i.Status).Size(16)
	t.Col(&i.Job).OnDelete(storm.SetNull)
}

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
	s, err := storm.Build(&dpJob{}, &dpIncident{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_delete_batch_probe_live"
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

// job writes a failed job, marked deleted or not, with incidents in the
// given states.
func job(t *testing.T, marked bool, incidents ...string) [16]byte {
	t.Helper()
	ctx := context.Background()
	c := dpjob.Create()
	c.SetStatus("failed")
	r, err := c.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if marked {
		if err := dpjob.Delete(ctx, ex, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range incidents {
		i := dpincident.Create()
		i.SetJobID(r.ID)
		i.SetStatus(status)
		if _, err := i.Insert(ctx, ex); err != nil {
			t.Fatal(err)
		}
	}
	return r.ID
}

func clear(t *testing.T) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "DELETE FROM dp_incidents; DELETE FROM dp_jobs"); err != nil {
		t.Fatal(err)
	}
}

func left(t *testing.T) map[[16]byte]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT id FROM dp_jobs")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[[16]byte]bool{}
	for rows.Next() {
		var id [16]byte
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	return out
}

func unblocked() store.DpJobIncidentsProbeQuery {
	return store.DpJobNotHavingIncidents(dpjob.New().Where(dpjob.Status.Eq("failed")),
		dpincident.Status.NotEq("resolved"))
}

func TestTheProbePicksWhatIsPurged(t *testing.T) {
	clear(t)
	ctx := context.Background()
	job(t, false)
	job(t, false, "resolved")
	job(t, true, "resolved")
	open := job(t, false, "open", "resolved")
	n, err := unblocked().HardDeleteBatch(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	got := left(t)
	if n != 3 || len(got) != 1 || !got[open] {
		t.Errorf("purged %d and left %v; want every job but the one an open incident names, the marked one too", n, got)
	}
}

func TestTheLimitBoundsABatch(t *testing.T) {
	clear(t)
	for i := 0; i < 5; i++ {
		job(t, false)
	}
	ctx := context.Background()
	var total, runs int64
	for {
		h := store.DpJobNotHavingIncidents(dpjob.New().Where(dpjob.Status.Eq("failed")).Limit(2),
			dpincident.Status.NotEq("resolved"))
		n, err := h.HardDeleteBatch(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		total += n
		runs++
		if n < 2 {
			break
		}
	}
	if total != 5 || runs != 3 {
		t.Errorf("purged %d in %d runs, want 5 in 3", total, runs)
	}
}

func TestALockedOrOffsetProbeIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, q := range []dpjob.Query{
		dpjob.New().Where(dpjob.Status.Eq("failed")).ForUpdate(),
		dpjob.New().Where(dpjob.Status.Eq("failed")).Offset(1),
	} {
		if _, err := store.DpJobNotHavingIncidents(q).HardDeleteBatch(ctx, ex); !errors.Is(err, runtime.ErrPagedWrite) {
			t.Errorf("returned %v, want ErrPagedWrite", err)
		}
	}
}
`
