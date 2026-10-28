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

// A probe on matching columns, and UpdateAll over it: "put back on offer
// every task out of retries with no open fault at its step" is one UPDATE
// whose EXISTS is correlated on the run AND the step, which no foreign key
// describes.

type poRun struct {
	storm.Model
	Name string
}

func (r *poRun) Schema(t *storm.Table) { t.Col(&r.Name).Size(16) }

type poTask struct {
	storm.Model
	Run     poRun
	NodeID  string
	Retries int
}

func (e *poTask) Schema(t *storm.Table) {
	t.Col(&e.NodeID).Size(32)
	var f poFault
	t.ProbesOn("StepFaults", &f, &e.Run, &f.Run, &e.NodeID, &f.NodeID)
}

type poFault struct {
	storm.Model
	Run       poRun
	NodeID    string
	Status    string
	DeletedAt *time.Time
}

func (f *poFault) Schema(t *storm.Table) {
	t.Col(&f.NodeID).Size(32)
	t.Col(&f.Status).Size(16)
	t.SoftDelete(&f.DeletedAt)
}

func TestProbesOnAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&poRun{}, &poTask{}, &poFault{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "polive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(probesOnLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	src = strings.ReplaceAll(src, "MODELS", probesOnModels)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a probe on matching columns does not select what it says:\n%s", out)
	}
	for _, name := range []string{
		"TestThePairsCorrelate",
		"TestUpdateAllOverTheAntiJoin",
		"TestUpdateAllRefusesAShapedRead",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

func TestProbesOnWantsPairsOfFields(t *testing.T) {
	if _, err := storm.Build(&poRun{}, &poOdd{}, &poFault{}); err == nil || !strings.Contains(err.Error(), "pairs") {
		t.Errorf("an odd ProbesOn built: %v", err)
	}
}

type poOdd struct {
	storm.Model
	NodeID string
}

func (o *poOdd) Schema(t *storm.Table) {
	var f poFault
	t.ProbesOn("Faults", &f, &o.NodeID)
}

const probesOnModels = `type poRun struct {
	storm.Model
	Name string
}

func (r *poRun) Schema(t *storm.Table) { t.Col(&r.Name).Size(16) }

type poTask struct {
	storm.Model
	Run     poRun
	NodeID  string
	Retries int
}

func (e *poTask) Schema(t *storm.Table) {
	t.Col(&e.NodeID).Size(32)
	var f poFault
	t.ProbesOn("StepFaults", &f, &e.Run, &f.Run, &e.NodeID, &f.NodeID)
}

type poFault struct {
	storm.Model
	Run       poRun
	NodeID    string
	Status    string
	DeletedAt *time.Time
}

func (f *poFault) Schema(t *storm.Table) {
	t.Col(&f.NodeID).Size(32)
	t.Col(&f.Status).Size(16)
	t.SoftDelete(&f.DeletedAt)
}
`

const probesOnLiveSrc = `package store_test

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
	"IMPORTPATH/pofault"
	"IMPORTPATH/porun"
	"IMPORTPATH/potask"
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
	s, err := storm.Build(&poRun{}, &poTask{}, &poFault{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_probes_on_live"
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

// seed writes two runs and four tasks out of retries: (a, s1) with an open
// fault at its step, (a, s2) with an open fault at another run's s2 only,
// (b, s1) with a resolved fault, (b, s2) with an open fault marked deleted.
func seed(t *testing.T) (ids map[string][16]byte) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM po_faults; DELETE FROM po_tasks; DELETE FROM po_runs"); err != nil {
		t.Fatal(err)
	}
	runs := map[string][16]byte{}
	for _, name := range []string{"a", "b", "c"} {
		r := porun.Create()
		r.SetName(name)
		row, err := r.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		runs[name] = row.ID
	}
	ids = map[string][16]byte{}
	for _, k := range []struct{ run, node string }{{"a", "s1"}, {"a", "s2"}, {"b", "s1"}, {"b", "s2"}} {
		e := potask.Create()
		e.SetRunID(runs[k.run])
		e.SetNodeID(k.node)
		e.SetRetries(0)
		row, err := e.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		ids[k.run+k.node] = row.ID
	}
	for _, f := range []struct{ run, node, status string; gone bool }{
		{"a", "s1", "open", false}, {"c", "s2", "open", false},
		{"b", "s1", "resolved", false}, {"b", "s2", "open", true},
	} {
		in := pofault.Create()
		in.SetRunID(runs[f.run])
		in.SetNodeID(f.node)
		in.SetStatus(f.status)
		row, err := in.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		if f.gone {
			if err := pofault.Delete(ctx, ex, row.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	return ids
}

func TestThePairsCorrelate(t *testing.T) {
	ids := seed(t)
	ctx := context.Background()
	got, err := store.PoTaskHavingStepFaults(potask.New(), pofault.Status.Eq("open")).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != ids["as1"] {
		t.Errorf("found %+v, want only the task whose run and step hold an open live fault", got)
	}
}

func TestUpdateAllOverTheAntiJoin(t *testing.T) {
	ids := seed(t)
	ctx := context.Background()
	var m potask.Mut
	m.SetRetries(1)
	n, err := store.PoTaskNotHavingStepFaults(potask.New().Where(potask.Retries.Lte(0)),
		pofault.Status.Eq("open")).UpdateAll(ctx, ex, m)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := potask.New().Where(potask.Retries.Eq(1)).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	put := map[[16]byte]bool{}
	for _, r := range rows {
		put[r.ID] = true
	}
	if n != 3 || len(put) != 3 || put[ids["as1"]] {
		t.Errorf("re-offered %d (%v); want every task but the one an open live fault holds", n, put)
	}
}

func TestUpdateAllRefusesAShapedRead(t *testing.T) {
	seed(t)
	ctx := context.Background()
	var m potask.Mut
	m.SetRetries(5)
	for _, q := range []potask.Query{potask.New().Limit(1), potask.New().Offset(1), potask.New().ForUpdate()} {
		if _, err := store.PoTaskHavingStepFaults(q).UpdateAll(ctx, ex, m); !errors.Is(err, runtime.ErrPagedWrite) {
			t.Errorf("returned %v, want ErrPagedWrite", err)
		}
	}
	if n, err := store.PoTaskHavingStepFaults(potask.New()).UpdateAll(ctx, ex, potask.Mut{}); err != nil || n != 0 {
		t.Errorf("nothing assigned wrote %d (%v)", n, err)
	}
	_ = time.Second
}
`
