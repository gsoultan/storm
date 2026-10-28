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

// Where running work is sitting: an aggregation over a table, the table its
// rows reference, and the elements of a JSON array each row holds, grouped by
// the referenced table's key and an element's field, with a subtotal per key.

type awDef struct {
	storm.Model
	Key       string
	Name      string
	Version   int64
	DeletedAt *time.Time
}

func (d *awDef) Schema(t *storm.Table) {
	t.Col(&d.Key).Size(16)
	t.Col(&d.Name).Size(32)
	t.SoftDelete(&d.DeletedAt)
}

type awInst struct {
	storm.Model
	Def       *awDef
	Status    string
	Tokens    storm.JSON
	DeletedAt *time.Time
}

func (i *awInst) Schema(t *storm.Table) {
	t.Col(&i.Status).Size(16)
	t.SoftDelete(&i.DeletedAt)
}

func (i *awInst) Aggregates(a *storm.Aggregates) {
	var d awDef
	w := a.Named("Waiting").
		Join(&d, &i.Def).
		Elements("t", &i.Tokens).
		Where(a.Ne(a.Coalesce(a.Field("t", "status"), a.Lit("")), a.Lit("completed")))
	w.By(&d.Key)
	node := w.ByExpr("Node", a.Field("t", "node_id"))
	w.Sets([]string{"Key", "Node"}, []string{"Key"})
	w.FirstOf(&d.Name, "Name", a.Over().OrderByDesc(&d.Version))
	w.Count("Waiting")
	w.CountDistinct(&i.ID, "Instances")
	w.GroupingOf("Total", node)
}

func TestAggregateOverAJoinAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&awDef{}, &awInst{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "awlive" + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	if os.Getenv("STORM_KEEP") == "" {
		t.Cleanup(func() { os.RemoveAll(dir) })
	}

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
	src := strings.ReplaceAll(aggJoinLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	src = strings.ReplaceAll(src, "MODELS", aggJoinModels)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the aggregation over a join does not count what it says:\n%s", out)
	}
	for _, name := range []string{
		"TestWaitingWorkIsCountedPerKeyAndNode",
		"TestTheCallSiteNarrowsTheRows",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

func TestAggregateSourcesComeFirst(t *testing.T) {
	for name, m := range map[string]any{
		"a join after a grouping":   &awLate{},
		"a field with no range":     &awNoRange{},
		"elements of a text column": &awText{},
		"a join on no foreign key":  &awNoKey{},
	} {
		_, err := storm.Build(&awDef{}, m)
		t.Logf("%s: %v", name, err)
		if err == nil {
			t.Errorf("%s built", name)
		}
	}
}

type awLate struct {
	storm.Model
	Def *awDef
}

func (i *awLate) Aggregates(a *storm.Aggregates) {
	var d awDef
	a.Named("X").By(&i.ID).Join(&d, &i.Def).Count("N")
}

type awNoRange struct {
	storm.Model
	Tokens storm.JSON
}

func (i *awNoRange) Aggregates(a *storm.Aggregates) {
	a.Named("X").ByExpr("Node", a.Field("t", "node_id"))
}

type awText struct {
	storm.Model
	Notes string
}

func (i *awText) Aggregates(a *storm.Aggregates) {
	a.Named("X").Elements("t", &i.Notes).Count("N")
}

type awNoKey struct {
	storm.Model
	Status string
}

func (i *awNoKey) Aggregates(a *storm.Aggregates) {
	var d awDef
	a.Named("X").Join(&d, &i.Status).Count("N")
}

const aggJoinModels = `type awDef struct {
	storm.Model
	Key       string
	Name      string
	Version   int64
	DeletedAt *time.Time
}

func (d *awDef) Schema(t *storm.Table) {
	t.Col(&d.Key).Size(16)
	t.Col(&d.Name).Size(32)
	t.SoftDelete(&d.DeletedAt)
}

type awInst struct {
	storm.Model
	Def       *awDef
	Status    string
	Tokens    storm.JSON
	DeletedAt *time.Time
}

func (i *awInst) Schema(t *storm.Table) {
	t.Col(&i.Status).Size(16)
	t.SoftDelete(&i.DeletedAt)
}
`

const aggJoinLiveSrc = `package store_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"

	"IMPORTPATH/awdef"
	"IMPORTPATH/awinst"
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
	s, err := storm.Build(&awDef{}, &awInst{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_aggregate_join_live"
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

// seed writes k1 at two versions and k2 at one, and their instances:
//
//	A  k1 v1 active     a waiting, b completed
//	B  k1 v2 active     a, a, and one with no node
//	C  k2    suspended  an object, not an array
//	D  k2    done       x
//	E  k2    active     y, but the instance is deleted
//	F  k2    active     z, its status null
func seed() {
	ctx := context.Background()
	def := func(key, name string, version int64) [16]byte {
		d := awdef.Create()
		d.SetKey(key)
		d.SetName(name)
		d.SetVersion(version)
		row, err := d.Insert(ctx, ex)
		must(err)
		return row.ID
	}
	inst := func(def [16]byte, status, tokens string, gone bool) {
		i := awinst.Create()
		i.SetDefID(def)
		i.SetStatus(status)
		i.SetTokens([]byte(tokens))
		if gone {
			i.SetDeletedAt(time.Now())
		}
		_, err := i.Insert(ctx, ex)
		must(err)
	}
	k1v1, k1v2 := def("k1", "K one", 1), def("k1", "K two", 2)
	k2 := def("k2", "Two", 1)
	inst(k1v1, "active", ` + "`" + `[{"node_id":"a","status":"waiting"},{"node_id":"b","status":"completed"}]` + "`" + `, false)
	inst(k1v2, "active", ` + "`" + `[{"node_id":"a"},{"node_id":"a","status":"waiting"},{"status":"waiting"}]` + "`" + `, false)
	inst(k2, "suspended", ` + "`" + `{"node_id":"c"}` + "`" + `, false)
	inst(k2, "done", ` + "`" + `[{"node_id":"x"}]` + "`" + `, false)
	inst(k2, "active", ` + "`" + `[{"node_id":"y"}]` + "`" + `, true)
	inst(k2, "active", ` + "`" + `[{"node_id":"z","status":null}]` + "`" + `, false)
}

func describe(rows []awinst.WaitingRow) string {
	var out []string
	for _, r := range rows {
		node := "-"
		if r.Total == 0 {
			node = "(none)"
			if n, ok := r.Node.Get(); ok {
				node = n
			}
		}
		out = append(out, fmt.Sprintf("%s/%s:%d/%d %s", r.Key.V, node, r.Waiting, r.Instances, r.Name.V))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func TestWaitingWorkIsCountedPerKeyAndNode(t *testing.T) {
	rows, err := awinst.New().Where(awinst.Status.In("active", "suspended")).AllWaiting(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	want := "k1/(none):1/1 K two k1/-:4/2 K two k1/a:3/2 K two k2/-:1/1 Two k2/z:1/1 Two"
	if got := describe(rows); got != want {
		t.Errorf("waiting:\n got %s\nwant %s", got, want)
	}
}

func TestTheCallSiteNarrowsTheRows(t *testing.T) {
	rows, err := awinst.New().Where(awinst.Status.Eq("done")).AllWaiting(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := describe(rows), "k2/-:1/1 Two k2/x:1/1 Two"; got != want {
		t.Errorf("done:\n got %s\nwant %s", got, want)
	}
}
`
