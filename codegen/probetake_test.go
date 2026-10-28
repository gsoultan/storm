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

// A probe through a to-one reference, and Take over it: "claim the oldest
// pending delivery whose webhook is still active" is one UPDATE bounded by
// one read that holds the probe, the order, the Limit and the row lock.

type ptHook struct {
	storm.Model
	Active    bool
	DeletedAt *time.Time
}

func (h *ptHook) Schema(t *storm.Table) { t.SoftDelete(&h.DeletedAt) }

type ptDelivery struct {
	storm.Model
	Hook   ptHook
	Status string
}

func (d *ptDelivery) Schema(t *storm.Table) {
	t.Col(&d.Status).Size(16)
	t.Takes()
	t.Probes(&d.Hook)
}

func (d *ptDelivery) Aggregates(a *storm.Aggregates) {
	byHook := a.Named("ByHook")
	byHook.By(&d.Hook)
	byHook.Count("Total")
}

func TestTakeOverAToOneProbeAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&ptHook{}, &ptDelivery{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "ptlive" + strconv.Itoa(os.Getpid())
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
	src := strings.ReplaceAll(probeTakeLiveSrc, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base)
	if err := os.WriteFile(filepath.Join(dir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Take over a to-one probe does not claim what the read selects:\n%s", out)
	}
	for _, name := range []string{
		"TestTheProbeNarrowsTheClaim",
		"TestTwoWorkersClaimTwoRows",
		"TestTheAntiJoinClaimsTheRest",
		"TestNothingAssignedOrAnOffset",
		"TestTheGroupsCountWhatTheProbeSelects",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

func TestProbesRefusesAToManyOrUnknownField(t *testing.T) {
	s, err := storm.Build(&dpJob{}, &dpIncident{})
	if err != nil {
		t.Fatal(err)
	}
	s.Table("dp_jobs").Probes = []string{"Incidents"}
	if _, err := codegen.Package(s, codegen.PackageOptions{
		Dir: t.TempDir(), Import: "github.com/gsoultan/storm",
		Package: "store", PackageImport: "example.com/store",
	}); err == nil || !strings.Contains(err.Error(), "to-one") {
		t.Errorf("a to-many named by Probes generated: %v", err)
	}
}

const probeTakeLiveSrc = `package store_test

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
	"IMPORTPATH/ptdelivery"
	"IMPORTPATH/pthook"
)

type ptHook struct {
	storm.Model
	Active    bool
	DeletedAt *time.Time
}

func (h *ptHook) Schema(t *storm.Table) { t.SoftDelete(&h.DeletedAt) }

type ptDelivery struct {
	storm.Model
	Hook   ptHook
	Status string
}

func (d *ptDelivery) Schema(t *storm.Table) {
	t.Col(&d.Status).Size(16)
	t.Takes()
	t.Probes(&d.Hook)
}

func (d *ptDelivery) Aggregates(a *storm.Aggregates) {
	byHook := a.Named("ByHook")
	byHook.By(&d.Hook)
	byHook.Count("Total")
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
	s, err := storm.Build(&ptHook{}, &ptDelivery{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_probe_take_live"
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

// seed empties both tables and writes three hooks, an active one, an
// inactive one and an active one marked deleted, then a pending delivery
// to each in turn, a second apart, oldest first: five in all, two to the
// live active hook.
func seed(t *testing.T) (live [16]byte, ids [][16]byte) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM pt_deliveries; DELETE FROM pt_hooks"); err != nil {
		t.Fatal(err)
	}
	var hooks [3][16]byte
	for i, active := range []bool{true, false, true} {
		h := pthook.Create()
		h.SetActive(active)
		r, err := h.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		hooks[i] = r.ID
	}
	if err := pthook.Delete(ctx, ex, hooks[2]); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Hour).UTC()
	for i, h := range []int{1, 0, 2, 0, 1} {
		id := [16]byte{0xd, byte(i + 1), 15: 0xd}
		if _, err := pool.Exec(ctx, "INSERT INTO pt_deliveries (id, hook_id, status, created_at, updated_at) VALUES ($1, $2, 'pending', $3, $3)",
			id, hooks[h], start.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return hooks[0], ids
}

func claim() ptdelivery.Mut {
	var m ptdelivery.Mut
	m.SetStatus("claimed")
	return m
}

func next() ptdelivery.Query {
	return ptdelivery.New().Where(ptdelivery.Status.Eq("pending")).
		Order(ptdelivery.CreatedAt.Asc()).Limit(1).ForUpdateSkipLocked()
}

func TestTheProbeNarrowsTheClaim(t *testing.T) {
	_, ids := seed(t)
	ctx := context.Background()
	h := store.PtDeliveryHavingHook(next(), pthook.Active.Eq(true))
	for _, want := range []int{1, 3} {
		got, err := h.Take(ctx, ex, claim(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != ids[want] || got[0].Status != "claimed" {
			t.Fatalf("claimed %+v, want delivery %d, claimed", got, want)
		}
	}
	got, err := h.Take(ctx, ex, claim(), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("claimed %+v (%v) once the live active hook's were gone, want none", got, err)
	}
}

func TestTwoWorkersClaimTwoRows(t *testing.T) {
	_, ids := seed(t)
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
	h := store.PtDeliveryHavingHook(next(), pthook.Active.Eq(true))
	first, err := h.Take(ctx, pgxdrv.Conn{C: a}, claim(), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Take(ctx, pgxdrv.Conn{C: b}, claim(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].ID != ids[1] || second[0].ID != ids[3] {
		t.Errorf("took %+v then %+v, want the live active hook's two, oldest first", first, second)
	}
}

func TestTheAntiJoinClaimsTheRest(t *testing.T) {
	_, ids := seed(t)
	ctx := context.Background()
	// No live active hook: the inactive one's, and the deleted one's,
	// because a marked row is no row to the probe.
	h := store.PtDeliveryNotHavingHook(ptdelivery.New().Where(ptdelivery.Status.Eq("pending")).
		Order(ptdelivery.CreatedAt.Asc()).Limit(10), pthook.Active.Eq(true))
	got, err := h.Take(ctx, ex, claim(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[[16]byte]bool{ids[0]: true, ids[2]: true, ids[4]: true}
	if len(got) != 3 {
		t.Fatalf("claimed %d, want 3", len(got))
	}
	for _, r := range got {
		if !want[r.ID] {
			t.Errorf("claimed %v, which a live active hook names", r.ID)
		}
	}
}

func TestNothingAssignedOrAnOffset(t *testing.T) {
	seed(t)
	ctx := context.Background()
	h := store.PtDeliveryHavingHook(next(), pthook.Active.Eq(true))
	got, err := h.Take(ctx, ex, ptdelivery.Mut{}, nil)
	if err != nil || len(got) != 0 {
		t.Errorf("nothing assigned returned %+v, %v", got, err)
	}
	paged := store.PtDeliveryHavingHook(ptdelivery.New().Offset(1), pthook.Active.Eq(true))
	if _, err := paged.Take(ctx, ex, claim(), nil); !errors.Is(err, runtime.ErrPagedWrite) {
		t.Errorf("returned %v, want ErrPagedWrite", err)
	}
	n, err := ptdelivery.New().Where(ptdelivery.Status.Eq("claimed")).Count(ctx, ex)
	if err != nil || n != 0 {
		t.Errorf("%d rows claimed by calls that should write nothing (%v)", n, err)
	}
}

func TestTheGroupsCountWhatTheProbeSelects(t *testing.T) {
	live, _ := seed(t)
	ctx := context.Background()
	got, err := store.PtDeliveryHavingHook(ptdelivery.New().Where(ptdelivery.Status.Eq("pending")),
		pthook.Active.Eq(true)).AllByHook(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].HookID != live || got[0].Total != 2 {
		t.Errorf("grouped %+v, want the live active hook's two", got)
	}
	all, err := store.PtDeliveryNotHavingHook(ptdelivery.New(), pthook.Active.Eq(true)).AllByHook(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, g := range all {
		n += g.Total
	}
	if len(all) != 2 || n != 3 {
		t.Errorf("the anti-join grouped %+v, want two hooks and three deliveries", all)
	}
	for _, q := range []ptdelivery.Query{
		ptdelivery.New().Order(ptdelivery.CreatedAt.Asc()),
		ptdelivery.New().ForUpdate(),
		ptdelivery.New().Offset(1),
	} {
		if _, err := store.PtDeliveryHavingHook(q).AllByHook(ctx, ex); !errors.Is(err, runtime.ErrGroupedRead) {
			t.Errorf("returned %v, want ErrGroupedRead", err)
		}
	}
}
`
