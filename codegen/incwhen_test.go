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

// A guarded increment: "claim the job, and count the claim as an attempt
// only if it takes over a running one" is one UPDATE whose counter reads
// the status the same statement replaces.

type iwJob struct {
	storm.Model
	Status  string
	Retries int
}

func (j *iwJob) Schema(t *storm.Table) {
	t.Col(&j.Status).Size(16)
	t.Col(&j.Retries).IncWhen(&j.Status)
}

func TestIncWhenAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&iwJob{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "iwlive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(incWhenLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	src = strings.ReplaceAll(src, "MODELS", incWhenModels)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a guarded increment does not count what it says:\n%s", out)
	}
	for _, name := range []string{
		"TestTheGuardReadsTheReplacedValue",
		"TestUpdateReadsTheCounterBack",
		"TestALaterSetterDropsTheGuard",
		"TestTheBatchOpBindsTheGuard",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

func TestIncWhenRefusesWhatItCannotWrite(t *testing.T) {
	if _, err := storm.Build(&iwSelf{}); err == nil || !strings.Contains(err.Error(), "itself") {
		t.Errorf("a guard on its own counter built: %v", err)
	}
	for _, m := range []any{&iwText{}, &iwJSON{}} {
		s, err := storm.Build(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := codegen.Package(s, codegen.PackageOptions{
			Dir: t.TempDir(), Import: "github.com/gsoultan/storm",
			Package: "store", PackageImport: "example.com/store",
		}); err == nil || !strings.Contains(err.Error(), "IncWhen") {
			t.Errorf("%T generated: %v", m, err)
		}
	}
}

// Every back end spells the guard in its own quoting and placeholder.
func TestIncWhenOnEveryBackEnd(t *testing.T) {
	s, err := storm.Build(&iwJob{})
	if err != nil {
		t.Fatal(err)
	}
	for d, want := range map[codegen.Dialect]string{
		codegen.DialectPostgres: `"retries" = "retries" + CASE WHEN "status" = $`,
		codegen.DialectMySQL:    "`retries` = `retries` + CASE WHEN `status` = ?",
		codegen.DialectMariaDB:  "`retries` = `retries` + CASE WHEN `status` = ?",
		codegen.DialectMSSQL:    `[retries] = [retries] + CASE WHEN [status] = @`,
		codegen.DialectOracle:   `"retries" = "retries" + CASE WHEN "status" = :`,
	} {
		files, err := codegen.Package(s, codegen.PackageOptions{
			Dir: t.TempDir(), Import: "github.com/gsoultan/storm", Dialect: d,
		})
		if err != nil {
			t.Fatalf("dialect %d: %v", d, err)
		}
		var all strings.Builder
		for _, b := range files {
			all.Write(b)
		}
		if !strings.Contains(all.String(), strconv.Quote(want)) || !strings.Contains(all.String(), "IncRetriesWhenStatus(v string)") {
			t.Errorf("dialect %d does not carry %s", d, want)
		}
	}
}

type iwSelf struct {
	storm.Model
	Retries int
}

func (j *iwSelf) Schema(t *storm.Table) { t.Col(&j.Retries).IncWhen(&j.Retries) }

type iwText struct {
	storm.Model
	Status string
	Note   string
}

func (j *iwText) Schema(t *storm.Table) { t.Col(&j.Note).IncWhen(&j.Status) }

type iwJSON struct {
	storm.Model
	Meta    map[string]any
	Retries int
}

func (j *iwJSON) Schema(t *storm.Table) { t.Col(&j.Retries).IncWhen(&j.Meta) }

const incWhenModels = `type iwJob struct {
	storm.Model
	Status  string
	Retries int
}

func (j *iwJob) Schema(t *storm.Table) {
	t.Col(&j.Status).Size(16)
	t.Col(&j.Retries).IncWhen(&j.Status)
}
`

const incWhenLiveSrc = `package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"

	"IMPORTPATH/iwjob"
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
	s, err := storm.Build(&iwJob{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_inc_when_live"
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

func job(t *testing.T, status string, retries int64) [16]byte {
	t.Helper()
	j := iwjob.Create()
	j.SetStatus(status)
	j.SetRetries(retries)
	row, err := j.Insert(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}

func retries(t *testing.T, id [16]byte) (string, int64) {
	t.Helper()
	rows, err := iwjob.New().Where(iwjob.ID.Eq(id)).All(context.Background(), ex, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read %d rows (%v)", len(rows), err)
	}
	return rows[0].Status, rows[0].Retries
}

// One statement over a pending and a running job: both end running, and
// only the one that already was counts the claim.
func TestTheGuardReadsTheReplacedValue(t *testing.T) {
	ctx := context.Background()
	pending, running := job(t, "pending", 0), job(t, "running", 2)
	var m iwjob.Mut
	m.SetStatus("running")
	m.IncRetriesWhenStatus("running")
	n, err := iwjob.New().Where(iwjob.ID.In(pending, running)).UpdateAll(ctx, ex, m)
	if err != nil || n != 2 {
		t.Fatalf("wrote %d (%v)", n, err)
	}
	if s, r := retries(t, pending); s != "running" || r != 0 {
		t.Errorf("the pending job is %s with %d retries, want running with 0", s, r)
	}
	if s, r := retries(t, running); s != "running" || r != 3 {
		t.Errorf("the running job is %s with %d retries, want running with 3", s, r)
	}
}

func TestUpdateReadsTheCounterBack(t *testing.T) {
	ctx := context.Background()
	id := job(t, "running", 4)
	m := iwjob.MutateKey(id)
	m.IncRetriesWhenStatus("running")
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if m.Row().Retries != 5 {
		t.Errorf("the staged row holds %d retries, want the 5 the database wrote", m.Row().Retries)
	}
}

func TestALaterSetterDropsTheGuard(t *testing.T) {
	ctx := context.Background()
	id := job(t, "running", 1)
	m := iwjob.MutateKey(id)
	m.IncRetriesWhenStatus("running")
	m.SetRetries(7)
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if _, r := retries(t, id); r != 7 {
		t.Errorf("retries is %d, want the 7 the last setter staged", r)
	}
	m = iwjob.MutateKey(id)
	m.SetRetries(1)
	m.IncRetriesWhenStatus("pending")
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if _, r := retries(t, id); r != 7 {
		t.Errorf("retries is %d, want 7: the guard staged last does not match", r)
	}
}

func TestTheBatchOpBindsTheGuard(t *testing.T) {
	ctx := context.Background()
	id := job(t, "running", 0)
	m := iwjob.MutateKey(id)
	m.SetStatus("done")
	m.IncRetriesWhenStatus("running")
	op, ok := m.UpdateOp()
	if !ok || !strings.Contains(op.SQL, "CASE WHEN") || strings.Count(op.SQL, "$") != len(op.Args) {
		t.Fatalf("%q binds %d args", op.SQL, len(op.Args))
	}
	if _, err := ex.Exec(ctx, op.SQL, op.Args); err != nil {
		t.Fatal(err)
	}
	if s, r := retries(t, id); s != "done" || r != 1 {
		t.Errorf("the job is %s with %d retries, want done with 1", s, r)
	}
	_ = storm.Model{}
}
`
