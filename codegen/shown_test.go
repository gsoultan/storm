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

// A list of keys, each shown as the version a release made live and as its
// newest when none is: one join over two CTEs, each narrowed by its own
// query, a second copy of the declaring table under an alias, a composite ON
// and a condition that reads a CTE's column.

type swDef struct {
	storm.Model
	Project   string
	Key       string
	Version   int64
	Name      string
	DeletedAt *time.Time
}

func (d *swDef) Schema(t *storm.Table) {
	t.Col(&d.Project).Size(16)
	t.Col(&d.Key).Size(16)
	t.Col(&d.Name).Size(32)
	t.SoftDelete(&d.DeletedAt)
}

func (d *swDef) Aggregates(a *storm.Aggregates) {
	k := a.Named("Keys")
	k.By(&d.Project, &d.Key)
	k.Max(&d.Version, "Newest")
}

func (d *swDef) Joins(j *storm.Joins) {
	var r swRelease
	var lv swDef
	j.Named("Shown").
		WithWhere("keys", d, "Keys").
		WithWhere("live", &r, "Live").
		InnerWith("keys", j.OnCols("keys", "project", &d.Project).And("keys", "key", &d.Key)).
		LeftWith("live", j.OnCols("live", "project", &d.Project).And("live", "key", &d.Key)).
		LeftAs("lv", &lv, j.OnCols("sw_defs", "project", &lv.Project).
			And("sw_defs", "key", &lv.Key).And("live", "version", &lv.Version)).
		Where(j.Eq(&d.Version, j.Coalesce(&lv.Version, j.ColOf("keys", "newest")))).
		Take(&d.Key, "Key").
		Take(&d.Version, "Version").
		Take(&d.Name, "Name").
		Take(&lv.Version, "LiveVersion").
		TakeFrom("keys", "newest", "Newest").
		OrderAsc(&d.Key).
		OrderAsc(&d.Project)
}

type swRelease struct {
	storm.Model
	Project    string
	Key        string
	Version    int64
	ActivateAt time.Time
	DeletedAt  *time.Time
}

func (r *swRelease) Schema(t *storm.Table) {
	t.Col(&r.Project).Size(16)
	t.Col(&r.Key).Size(16)
	t.SoftDelete(&r.DeletedAt)
}

func (r *swRelease) Aggregates(a *storm.Aggregates) {
	l := a.Named("Live")
	l.By(&r.Project, &r.Key)
	l.FirstOf(&r.Version, "Version", a.Over().OrderByDesc(&r.ActivateAt))
}

func TestNarrowedJoinAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&swDef{}, &swRelease{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "swlive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(shownLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	src = strings.ReplaceAll(src, "MODELS", shownModels)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the narrowed join does not read what it says:\n%s", out)
	}
	for _, name := range []string{
		"TestEachKeyShowsItsLiveVersionOrItsNewest",
		"TestTheNarrowingQueriesChooseTheRows",
		"TestTheJoinCountsAndPages",
		"TestTheJoinRefusesWhatItCannotPlace",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

func TestNarrowedJoinDeclarations(t *testing.T) {
	for name, m := range map[string]any{
		"the declaring model as an alias": &swSelf{},
		"two joins under one name":        &swTwice{},
		"a column no scope has":           &swNoColumn{},
	} {
		if _, err := storm.Build(&swRelease{}, m); err == nil {
			t.Errorf("%s built", name)
		}
	}
}

type swSelf struct {
	storm.Model
	Key string
}

func (d *swSelf) Joins(j *storm.Joins) {
	j.Named("X").LeftAs("again", d, j.OnCols("sw_selves", "key", &d.Key)).Take(&d.Key, "Key")
}

type swTwice struct {
	storm.Model
	Key string
}

func (d *swTwice) Joins(j *storm.Joins) {
	var a, b swRelease
	j.Named("X").
		LeftAs("r", &a, j.OnCols("sw_twices", "key", &a.Key)).
		LeftAs("r", &b, j.OnCols("sw_twices", "key", &b.Key)).
		Take(&d.Key, "Key")
}

type swNoColumn struct {
	storm.Model
	Key string
}

func (d *swNoColumn) Joins(j *storm.Joins) {
	var r swRelease
	j.Named("X").
		LeftAs("r", &r, j.OnCols("sw_no_columns", "key", &r.Key)).
		Where(j.Eq(&d.Key, j.ColOf("r", "nonesuch"))).
		Take(&d.Key, "Key")
}

const shownModels = `type swDef struct {
	storm.Model
	Project   string
	Key       string
	Version   int64
	Name      string
	DeletedAt *time.Time
}

func (d *swDef) Schema(t *storm.Table) {
	t.Col(&d.Project).Size(16)
	t.Col(&d.Key).Size(16)
	t.Col(&d.Name).Size(32)
	t.SoftDelete(&d.DeletedAt)
}

type swRelease struct {
	storm.Model
	Project    string
	Key        string
	Version    int64
	ActivateAt time.Time
	DeletedAt  *time.Time
}

func (r *swRelease) Schema(t *storm.Table) {
	t.Col(&r.Project).Size(16)
	t.Col(&r.Key).Size(16)
	t.SoftDelete(&r.DeletedAt)
}
`

const shownLiveSrc = `package store_test

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

	store "IMPORTPATH"
	"IMPORTPATH/swdef"
	"IMPORTPATH/swrelease"
)

MODELS

var (
	pool *pgxpool.Pool
	ex   runtime.Executor
	now  = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "STORM_DSN did not reach the generated package")
		os.Exit(1)
	}
	ctx := context.Background()
	s, err := storm.Build(&swDef{}, &swRelease{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_narrowed_join_live"
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

// seed writes p1's keys:
//
//	a  versions 1-3, v2 released an hour ago             shows v2, live 2
//	b  versions 1-2, v1 released for tomorrow            shows v2, newest
//	c  versions 1-2 with v2 deleted, v2 released          shows v1, the live one is gone
//	d  versions 1-2, v2 released, then v1 released later  shows v1, live 1
//	e  versions 1-2, v1 released and the release deleted shows v2
//
// and p2's key a, which no p1 read sees.
func seed() {
	ctx := context.Background()
	def := func(project, key string, version int64, gone bool) {
		d := swdef.Create()
		d.SetProject(project)
		d.SetKey(key)
		d.SetVersion(version)
		d.SetName(fmt.Sprintf("%s v%d", key, version))
		if gone {
			d.SetDeletedAt(now)
		}
		_, err := d.Insert(ctx, ex)
		must(err)
	}
	release := func(project, key string, version int64, at time.Time, gone bool) {
		r := swrelease.Create()
		r.SetProject(project)
		r.SetKey(key)
		r.SetVersion(version)
		r.SetActivateAt(at)
		if gone {
			r.SetDeletedAt(now)
		}
		_, err := r.Insert(ctx, ex)
		must(err)
	}
	for v := int64(1); v <= 3; v++ {
		def("p1", "a", v, false)
	}
	release("p1", "a", 2, now.Add(-time.Hour), false)
	def("p1", "b", 1, false)
	def("p1", "b", 2, false)
	release("p1", "b", 1, now.Add(24*time.Hour), false)
	def("p1", "c", 1, false)
	def("p1", "c", 2, true)
	release("p1", "c", 2, now.Add(-time.Hour), false)
	def("p1", "d", 1, false)
	def("p1", "d", 2, false)
	release("p1", "d", 2, now.Add(-2*time.Hour), false)
	release("p1", "d", 1, now.Add(-time.Hour), false)
	def("p1", "e", 1, false)
	def("p1", "e", 2, false)
	release("p1", "e", 1, now.Add(-time.Hour), true)
	def("p2", "a", 1, false)
	release("p2", "a", 1, now.Add(-time.Hour), false)
}

func p1() store.SwDefShownQuery {
	return store.SwDefShown(
		swdef.New().Where(swdef.Project.Eq("p1")),
		swdef.New().Where(swdef.Project.Eq("p1")),
		swrelease.New().Where(swrelease.Project.Eq("p1"), swrelease.ActivateAt.Lte(now)),
	)
}

func shown(rows []swdef.ShownRow) string {
	out := ""
	for _, r := range rows {
		live := "-"
		if v, ok := r.LiveVersion.Get(); ok {
			live = fmt.Sprint(v)
		}
		out += fmt.Sprintf("%s:v%d/live %s/newest %v ", r.Key, r.Version, live, r.Newest.V)
	}
	return out
}

func TestEachKeyShowsItsLiveVersionOrItsNewest(t *testing.T) {
	rows, err := p1().All(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	want := "a:v2/live 2/newest 3 b:v2/live -/newest 2 c:v1/live -/newest 1 d:v1/live 1/newest 2 e:v2/live -/newest 2 "
	if got := shown(rows); got != want {
		t.Errorf("shown:\n got %s\nwant %s", got, want)
	}
	if rows[0].Name != "a v2" {
		t.Errorf("a's name is %q, want the live version's", rows[0].Name)
	}
}

func TestTheNarrowingQueriesChooseTheRows(t *testing.T) {
	// No release narrowing by time: b's release for tomorrow is live too.
	rows, err := store.SwDefShown(
		swdef.New().Where(swdef.Project.Eq("p1"), swdef.Key.In("a", "b")),
		swdef.New().Where(swdef.Project.Eq("p1")),
		swrelease.New().Where(swrelease.Project.Eq("p1")),
	).All(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := shown(rows), "a:v2/live 2/newest 3 b:v1/live 1/newest 2 "; got != want {
		t.Errorf("shown:\n got %s\nwant %s", got, want)
	}
	// Both projects: p2's a is listed after p1's.
	rows, err = store.SwDefShown(swdef.New(), swdef.New(), swrelease.New().Where(swrelease.ActivateAt.Lte(now))).
		All(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 || rows[1].Key != "a" || rows[1].Version != 1 {
		t.Errorf("both projects: %s", shown(rows))
	}
}

func TestTheJoinCountsAndPages(t *testing.T) {
	ctx := context.Background()
	n, err := p1().Count(ctx, ex)
	if err != nil || n != 5 {
		t.Fatalf("count = %d, %v; want 5", n, err)
	}
	h := store.SwDefShown(
		swdef.New().Where(swdef.Project.Eq("p1")).Limit(2).Offset(1),
		swdef.New().Where(swdef.Project.Eq("p1")),
		swrelease.New().Where(swrelease.Project.Eq("p1"), swrelease.ActivateAt.Lte(now)),
	)
	rows, err := h.All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := shown(rows), "b:v2/live -/newest 2 c:v1/live -/newest 1 "; got != want {
		t.Errorf("page:\n got %s\nwant %s", got, want)
	}
	if n, err := h.Count(ctx, ex); err != nil || n != 5 {
		t.Errorf("a paged query's count = %d, %v; want 5", n, err)
	}
	h = store.SwDefShown(
		swdef.New().Where(swdef.Project.Eq("p1"), swdef.Key.In("a", "d")),
		swdef.New().Where(swdef.Project.Eq("p1")),
		swrelease.New().Where(swrelease.Project.Eq("p1"), swrelease.ActivateAt.Lte(now)),
	)
	if n, err := h.Count(ctx, ex); err != nil || n != 2 {
		t.Errorf("a filtered count = %d, %v; want 2", n, err)
	}
}

func TestTheJoinRefusesWhatItCannotPlace(t *testing.T) {
	ctx := context.Background()
	for name, h := range map[string]store.SwDefShownQuery{
		"an order": store.SwDefShown(swdef.New().Order(swdef.Key.Asc()), swdef.New(), swrelease.New()),
		"a lock":   store.SwDefShown(swdef.New().ForUpdate(), swdef.New(), swrelease.New()),
		"a CTE limit": store.SwDefShown(swdef.New(), swdef.New().Limit(5), swrelease.New()),
		"a CTE order": store.SwDefShown(swdef.New(), swdef.New(), swrelease.New().Order(swrelease.Key.Asc())),
	} {
		if _, err := h.All(ctx, ex); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
`
