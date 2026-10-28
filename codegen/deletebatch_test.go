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

// DeleteBatch removes exactly the rows a read would return, at most its
// Limit, in one statement: the bounded purge a sweep repeats.

type dbLog struct {
	storm.Model
	Kind string
	At   time.Time
}

func (l *dbLog) Schema(t *storm.Table) {
	t.Col(&l.Kind).Size(16)
	t.Takes()
}

func TestDeleteBatchRemovesTheRowsAReadWouldReturnAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&dbLog{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "dblive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(deleteBatchLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("DeleteBatch does not remove the rows the read selects:\n%s", out)
	}
	for _, name := range []string{
		"TestDeleteBatchRemovesAtMostTheLimitInTheReadsOrder",
		"TestDeleteBatchLeavesWhatTheReadDoesNotMatch",
		"TestASweepOfBatchesRemovesEverything",
		"TestDeleteBatchRefusesAnOffsetAndNoPredicate",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

// Only where a table asks for Take. On a soft-delete table it is
// HardDeleteBatch, which purges marked rows and live ones alike, as
// HardDeleteAll does.
func TestDeleteBatchIsAskedForAndHardOnASoftDeleteTable(t *testing.T) {
	for _, c := range []struct {
		model any
		want  string
	}{{&dbLog{}, "DeleteBatch"}, {&tkJob{}, "HardDeleteBatch"}, {&adCounter{}, ""}} {
		s, err := storm.Build(c.model)
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
		got := ""
		for _, b := range files {
			for _, name := range []string{"DeleteBatch", "HardDeleteBatch"} {
				if strings.Contains(string(b), "func (q Query) "+name+"(") {
					got = name
				}
			}
		}
		if got != c.want {
			t.Errorf("%T has %q, want %q", c.model, got, c.want)
		}
	}
}

const deleteBatchLiveSrc = `package store_test

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

	"IMPORTPATH/dblog"
)

type dbLog struct {
	storm.Model
	Kind string
	At   time.Time
}

func (l *dbLog) Schema(t *storm.Table) {
	t.Col(&l.Kind).Size(16)
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
	s, err := storm.Build(&dbLog{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_delete_batch_live"
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

var start = time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

// seed writes n rows of a kind, a second apart, oldest first.
func seed(t *testing.T, kind string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		c := dblog.Create()
		c.SetKind(kind)
		c.SetAt(start.Add(time.Duration(i) * time.Second))
		if _, err := c.Insert(ctx, ex); err != nil {
			t.Fatal(err)
		}
	}
}

func clear(t *testing.T) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "DELETE FROM db_logs"); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, q dblog.Query) int64 {
	t.Helper()
	n, err := q.Count(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeleteBatchRemovesAtMostTheLimitInTheReadsOrder(t *testing.T) {
	clear(t)
	seed(t, "old", 5)
	ctx := context.Background()
	n, err := dblog.New().Where(dblog.Kind.Eq("old")).Order(dblog.At.Asc()).Limit(2).DeleteBatch(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("removed %d rows, want 2", n)
	}
	// The two oldest went: what is left starts at the third.
	left, err := dblog.New().Order(dblog.At.Asc()).Limit(1).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || !left[0].At.Equal(start.Add(2*time.Second)) {
		t.Errorf("the oldest row left is %+v, want the third", left)
	}
}

func TestDeleteBatchLeavesWhatTheReadDoesNotMatch(t *testing.T) {
	clear(t)
	seed(t, "old", 3)
	seed(t, "keep", 3)
	n, err := dblog.New().Where(dblog.Kind.Eq("old")).Limit(100).DeleteBatch(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("removed %d rows, want 3", n)
	}
	if kept := count(t, dblog.New().Where(dblog.Kind.Eq("keep"))); kept != 3 {
		t.Errorf("%d rows of the other kind are left, want 3", kept)
	}
}

func TestASweepOfBatchesRemovesEverything(t *testing.T) {
	clear(t)
	seed(t, "old", 7)
	ctx := context.Background()
	var total, runs int64
	for {
		n, err := dblog.New().Where(dblog.At.Lt(start.Add(time.Hour))).Limit(3).ForUpdateSkipLocked().DeleteBatch(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		total += n
		runs++
		if n < 3 {
			break
		}
	}
	if total != 7 || runs != 3 {
		t.Errorf("removed %d rows in %d runs, want 7 in 3", total, runs)
	}
	if left := count(t, dblog.New()); left != 0 {
		t.Errorf("%d rows are left", left)
	}
}

func TestDeleteBatchRefusesAnOffsetAndNoPredicate(t *testing.T) {
	ctx := context.Background()
	if _, err := dblog.New().Where(dblog.Kind.Eq("old")).Offset(1).DeleteBatch(ctx, ex); !errors.Is(err, runtime.ErrPagedWrite) {
		t.Errorf("DeleteBatch with an offset returned %v, want ErrPagedWrite", err)
	}
	if _, err := dblog.New().Limit(1).DeleteBatch(ctx, ex); !errors.Is(err, runtime.ErrUnfilteredWrite) {
		t.Errorf("DeleteBatch with no predicate returned %v, want ErrUnfilteredWrite", err)
	}
}
`
