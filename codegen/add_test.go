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

// Add<Col>(n) grows a counter by n in the database, as Inc grows it by one.
// Two writers who each add from the same read land both amounts; a value
// computed in Go and set would keep one.

type adCounter struct {
	storm.Model
	Name  string
	Hits  int64
	Turns int32
	Spare *int64
}

func (c *adCounter) Schema(t *storm.Table) {
	t.Col(&c.Name).Size(40)
}

// A nullable counter has no Add: NULL + n is NULL, which erases the count.
func TestANullableCounterHasNoAdd(t *testing.T) {
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
	var src string
	for rel, b := range files {
		if strings.HasSuffix(rel, "adcounter.gen.go") {
			src = string(b)
		}
	}
	if src == "" {
		t.Fatalf("no adcounter package among %d files", len(files))
	}
	for _, want := range []string{"func (m *Mut) AddHits(n int64)", "func (m *Mut) AddTurns(n int32)", "func (m *Mut) IncSpare()"} {
		if !strings.Contains(src, want) {
			t.Errorf("the generated package has no %q", want)
		}
	}
	if strings.Contains(src, "AddSpare") {
		t.Error("a nullable counter has an Add, which would write NULL over it")
	}
}

func TestAddLandsEveryWritersAmountAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&adCounter{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "adlive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(addLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Add does not grow the counter by the amount:\n%s", out)
	}
	for _, name := range []string{
		"TestTwoWritersFromOneReadBothLand",
		"TestUpdateAllAddsToEveryRowItMatches",
		"TestTheLastAssignmentToAColumnWins",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

const addLiveSrc = `package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"

	"IMPORTPATH/adcounter"
)

type adCounter struct {
	storm.Model
	Name  string
	Hits  int64
	Turns int32
	Spare *int64
}

func (c *adCounter) Schema(t *storm.Table) {
	t.Col(&c.Name).Size(40)
}

var (
	pool *pgxpool.Pool
	ex   runtime.Executor
	one  = [16]byte{1, 15: 1}
	two  = [16]byte{2, 15: 2}
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "STORM_DSN did not reach the generated package")
		os.Exit(1)
	}
	ctx := context.Background()
	s, err := storm.Build(&adCounter{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_add_live"
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

func reset(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM ad_counters"); err != nil {
		t.Fatal(err)
	}
	for _, id := range [][16]byte{one, two} {
		if err := adcounter.Insert(ctx, ex, &adcounter.Row{ID: id, Name: "c", Hits: 10, Turns: 1}); err != nil {
			t.Fatal(err)
		}
	}
}

func read(t *testing.T, id [16]byte) adcounter.Row {
	t.Helper()
	row, found, err := adcounter.New().Where(adcounter.ID.Eq(id)).One(context.Background(), ex)
	if err != nil || !found {
		t.Fatalf("read %v: found %v, %v", id, found, err)
	}
	return row
}

func TestTwoWritersFromOneReadBothLand(t *testing.T) {
	reset(t)
	ctx := context.Background()
	seen := read(t, one)
	a, b := adcounter.Mutate(seen), adcounter.Mutate(seen)
	a.AddHits(5)
	b.AddHits(7)
	b.AddTurns(-1)
	if err := a.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if got := a.Row().Hits; got != 15 {
		t.Errorf("the first writer reads back %d, want 15", got)
	}
	if err := b.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if got := b.Row().Hits; got != 22 {
		t.Errorf("the second writer reads back %d, want 22", got)
	}
	if got := read(t, one); got.Hits != 22 || got.Turns != 0 {
		t.Errorf("the row holds hits %d, turns %d; want 22, 0", got.Hits, got.Turns)
	}
	if got := read(t, two).Hits; got != 10 {
		t.Errorf("the other row moved to %d", got)
	}
}

func TestUpdateAllAddsToEveryRowItMatches(t *testing.T) {
	reset(t)
	ctx := context.Background()
	var m adcounter.Mut
	m.AddHits(3)
	m.SetName("both")
	n, err := adcounter.New().Where(adcounter.Name.Eq("c")).UpdateAll(ctx, ex, m)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("wrote %d rows, want 2", n)
	}
	for _, id := range [][16]byte{one, two} {
		if got := read(t, id); got.Hits != 13 || got.Name != "both" {
			t.Errorf("row %v holds %d, %q; want 13, both", id, got.Hits, got.Name)
		}
	}
}

func TestTheLastAssignmentToAColumnWins(t *testing.T) {
	reset(t)
	ctx := context.Background()
	m := adcounter.MutateKey(one)
	m.SetHits(100)
	m.AddHits(4)
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if got := read(t, one).Hits; got != 14 {
		t.Errorf("set then add left %d, want 14", got)
	}
	m = adcounter.MutateKey(one)
	m.AddHits(4)
	m.SetHits(100)
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if got := read(t, one).Hits; got != 100 {
		t.Errorf("add then set left %d, want 100", got)
	}
	m = adcounter.MutateKey(one)
	m.AddHits(4)
	m.IncHits()
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if got := read(t, one).Hits; got != 101 {
		t.Errorf("add then inc left %d, want 101", got)
	}
}
`
