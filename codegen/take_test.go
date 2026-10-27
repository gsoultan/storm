package codegen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
)

// Take updates exactly the rows a read would return and hands them back. With
// ForUpdateSkipLocked it is the queue claim: two workers take two rows.

type tkJob struct {
	storm.Model
	Status       string
	LeaseExpires *time.Time
	DeletedAt    *time.Time
}

func (j *tkJob) Schema(t *storm.Table) {
	t.Col(&j.Status).Size(16)
	t.SoftDelete(&j.DeletedAt)
	t.Takes()
}

func TestTakeClaimsTheRowsAReadWouldReturnAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&tkJob{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "tklive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(takeLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Take does not write the rows the read selects:\n%s", out)
	}
	for _, name := range []string{
		"TestTwoWorkersTakeTwoRows",
		"TestTakeFollowsTheReadsOrderAndLimit",
		"TestTakeLeavesADeletedRowAlone",
		"TestTakeRefusesAnOffset",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

const takeLiveSrc = `package store_test

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

	"IMPORTPATH/tkjob"
)

type tkJob struct {
	storm.Model
	Status       string
	LeaseExpires *time.Time
	DeletedAt    *time.Time
}

func (j *tkJob) Schema(t *storm.Table) {
	t.Col(&j.Status).Size(16)
	t.SoftDelete(&j.DeletedAt)
	t.Takes()
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
	s, err := storm.Build(&tkJob{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_take_live"
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

// seed writes n queued jobs, created a second apart, oldest first.
func seed(t *testing.T, n int) [][16]byte {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM tk_jobs"); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour).UTC()
	ids := make([][16]byte, n)
	for i := range ids {
		ids[i] = [16]byte{byte(i + 1), 15: byte(i + 1)}
		if _, err := pool.Exec(ctx, "INSERT INTO tk_jobs (id, status, created_at, updated_at) VALUES ($1, 'queued', $2, $2)",
			ids[i], start.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func claim(until time.Time) tkjob.Mut {
	var m tkjob.Mut
	m.SetLeaseExpires(until)
	m.SetStatus("running")
	return m
}

func queued() tkjob.Query {
	return tkjob.New().Where(tkjob.Status.Eq("queued")).Order(tkjob.CreatedAt.Asc()).Limit(1).ForUpdateSkipLocked()
}

func TestTwoWorkersTakeTwoRows(t *testing.T) {
	ids := seed(t, 3)
	ctx := context.Background()
	a, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback(ctx)
	b, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Rollback(ctx)
	until := time.Now().Add(time.Minute).UTC().Truncate(time.Microsecond)

	first, err := queued().Take(ctx, pgxdrv.Conn{C: a}, claim(until), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The second worker asks while the first still holds its row: it must
	// step over it rather than wait or take it again.
	second, err := queued().Take(ctx, pgxdrv.Conn{C: b}, claim(until), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("took %d and %d rows, want 1 each", len(first), len(second))
	}
	if first[0].ID != ids[0] || second[0].ID != ids[1] {
		t.Errorf("took %v then %v, want the oldest then the next", first[0].ID, second[0].ID)
	}
	if first[0].Status != "running" || !first[0].LeaseExpires.Valid || !first[0].LeaseExpires.V.Equal(until) {
		t.Errorf("the taken row is %+v, not as written", first[0])
	}
}

func TestTakeFollowsTheReadsOrderAndLimit(t *testing.T) {
	ids := seed(t, 4)
	ctx := context.Background()
	got, err := tkjob.New().Where(tkjob.Status.Eq("queued")).
		Order(tkjob.CreatedAt.Desc()).Limit(2).
		Take(ctx, ex, claim(time.Now().UTC()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("took %d rows, want 2", len(got))
	}
	taken := map[[16]byte]bool{got[0].ID: true, got[1].ID: true}
	if !taken[ids[3]] || !taken[ids[2]] {
		t.Errorf("took %v, want the two newest", taken)
	}
	left, err := tkjob.New().Where(tkjob.Status.Eq("queued")).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if left != 2 {
		t.Errorf("%d rows are still queued, want 2", left)
	}
}

func TestTakeLeavesADeletedRowAlone(t *testing.T) {
	ids := seed(t, 2)
	ctx := context.Background()
	if err := tkjob.Delete(ctx, ex, ids[0]); err != nil {
		t.Fatal(err)
	}
	got, err := queued().Take(ctx, ex, claim(time.Now().UTC()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != ids[1] {
		t.Errorf("took %+v, want the live row", got)
	}
}

func TestTakeRefusesAnOffset(t *testing.T) {
	seed(t, 2)
	_, err := queued().Offset(1).Take(context.Background(), ex, claim(time.Now().UTC()), nil)
	if !errors.Is(err, runtime.ErrPagedWrite) {
		t.Errorf("Take with an offset returned %v, want ErrPagedWrite", err)
	}
}
`

// A table that asks for none of them carries none of them: Take, Tally and
// an INSERT … SELECT are code most packages would never call.
func TestTakeTallyAndInsertSelectAreAskedFor(t *testing.T) {
	s, err := storm.Build(&adCounter{})
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
	insertPer := regexp.MustCompile(`func Insert\w+Per\w+\(`)
	for rel, b := range files {
		for _, unwanted := range []string{"func (q Query) Take(", "type Tally struct", "UpdateAllAfter", "SelectHead"} {
			if strings.Contains(string(b), unwanted) {
				t.Errorf("%s has %q, which the model did not ask for", rel, unwanted)
			}
		}
		if insertPer.Match(b) {
			t.Errorf("%s has an INSERT … SELECT nobody asked for", rel)
		}
	}
}
