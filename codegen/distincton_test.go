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

// DISTINCT ON: "each process's newest setting" is one read of the history,
// keeping the first row of each run of the order's leading terms.

type doSetting struct {
	storm.Model
	Project   string
	Key       string
	Days      int
	DeletedAt *time.Time
}

func (s *doSetting) Schema(t *storm.Table) {
	t.Col(&s.Project).Size(16)
	t.Col(&s.Key).Size(16)
	t.SoftDelete(&s.DeletedAt)
}

func TestDistinctOnAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&doSetting{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "dolive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(distinctOnLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	src = strings.ReplaceAll(src, "MODELS", distinctOnModels)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("DISTINCT ON does not keep what it says:\n%s", out)
	}
	for _, name := range []string{
		"TestEachKeysNewestLiveRow",
		"TestTheKeyIsTheLeadingTerms",
		"TestAllDistinctOnRefuses",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

const distinctOnModels = `type doSetting struct {
	storm.Model
	Project   string
	Key       string
	Days      int
	DeletedAt *time.Time
}

func (s *doSetting) Schema(t *storm.Table) {
	t.Col(&s.Project).Size(16)
	t.Col(&s.Key).Size(16)
	t.SoftDelete(&s.DeletedAt)
}
`

const distinctOnLiveSrc = `package store_test

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

	"IMPORTPATH/dosetting"
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
	s, err := storm.Build(&doSetting{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_distinct_on_live"
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
	seed()

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

// seed writes a history per (project, key), oldest first: p1/a 30 then 10,
// p1/b 5, p2/a 40 then 1 marked deleted.
func seed() {
	ctx := context.Background()
	at := time.Now().Add(-time.Hour)
	for _, r := range []struct {
		project, key string
		days         int64
		gone         bool
	}{{"p1", "a", 30, false}, {"p1", "a", 10, false}, {"p1", "b", 5, false}, {"p2", "a", 40, false}, {"p2", "a", 1, true}} {
		at = at.Add(time.Minute)
		c := dosetting.Create()
		c.SetProject(r.project)
		c.SetKey(r.key)
		c.SetDays(r.days)
		c.SetCreatedAt(at)
		row, err := c.Insert(ctx, ex)
		must(err)
		if r.gone {
			must(dosetting.Delete(ctx, ex, row.ID))
		}
	}
}

func TestEachKeysNewestLiveRow(t *testing.T) {
	rows, err := dosetting.New().
		Order(dosetting.Project.Asc(), dosetting.Key.Asc(), dosetting.CreatedAt.Desc()).
		AllDistinctOn(context.Background(), ex, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s/%s=%d", r.Project, r.Key, r.Days))
	}
	if fmt.Sprint(got) != "[p1/a=10 p1/b=5 p2/a=40]" {
		t.Errorf("kept %v, want each key's newest live row", got)
	}
}

func TestTheKeyIsTheLeadingTerms(t *testing.T) {
	rows, err := dosetting.New().Where(dosetting.Key.Eq("a")).
		Order(dosetting.Project.Asc(), dosetting.Days.Asc()).
		AllDistinctOn(context.Background(), ex, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Days != 10 || rows[1].Days != 40 {
		t.Errorf("kept %+v, want each project's smallest", rows)
	}
}

func TestAllDistinctOnRefuses(t *testing.T) {
	ctx := context.Background()
	q := dosetting.New().Order(dosetting.Project.Asc())
	for _, c := range []struct {
		q dosetting.Query
		n int
	}{{q, 0}, {q, 2}, {dosetting.New(), 1}, {q.ForUpdate(), 1}} {
		if _, err := c.q.AllDistinctOn(ctx, ex, c.n, nil); err == nil || errors.Is(err, context.Canceled) {
			t.Errorf("n=%d ran: %v", c.n, err)
		}
	}
}
`
