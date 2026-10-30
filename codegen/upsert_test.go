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

// A partial unique index is only inferrable WITH its predicate. Without it
// PostgreSQL raises SQLSTATE 42P10 — "there is no unique or exclusion
// constraint matching the ON CONFLICT specification" — at run time, on the
// first row that actually collides, which a test inserting distinct rows
// never reaches.
type upTenant struct {
	storm.Model
	Tenant    string
	Slug      string
	Email     string
	Name      string
	DeletedAt *string
}

func (m *upTenant) Schema(t *storm.Table) {
	t.Index(&m.Tenant, &m.Slug).Unique().Where("deleted_at IS NULL").Named("ix_live_slug")
	t.Unique(storm.Lower(&m.Email))
	t.Unique(&m.Name)
}

func TestUpsert_ConflictSpecs(t *testing.T) {
	src := find(t, gen(t, codegen.PackageOptions{}, &upTenant{}), "uptenant")

	for _, want := range []string{
		// The primary key, a plain unique constraint, an expression unique
		// index, and a partial one — each with the spec PostgreSQL infers from.
		`" ON CONFLICT (\"id\")"`,
		`" ON CONFLICT (\"name\")"`,
		`" ON CONFLICT ((lower(email)))"`,
		`" ON CONFLICT (\"tenant\", \"slug\") WHERE deleted_at IS NULL"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("no conflict spec %s", want)
		}
	}
	// And a method per target, named after the keys rather than the index.
	for _, want := range []string{
		"func (n *Ins) OnConflictID() *Ins",
		"func (n *Ins) OnConflictName() *Ins",
		"func (n *Ins) OnConflictLowerEmail() *Ins",
		"func (n *Ins) OnConflictTenantSlug() *Ins",
		"func (n *Ins) DoNothing() *Ins",
		"func (n *Ins) Op() (runtime.BatchOp, error)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// A plain key column is equal on both sides — that is why the row conflicted —
// so assigning it from EXCLUDED is noise. An EXPRESSION key is not: lower(email)
// matching does not make the emails equal, and the caller's spelling is what
// they meant to store.
func TestUpsert_KeyColumnsAreNotAssignedButExpressionKeysAre(t *testing.T) {
	src := find(t, gen(t, codegen.PackageOptions{}, &upTenant{}), "uptenant")

	// The row index is the position in conflictSpecs, so read the order out
	// of the generated code rather than assuming it.
	specs := between(t, src, "var conflictSpecs = []string{", "\n}")
	body := between(t, src, "var assignNames = ", "\n}")
	rows := strings.Split(body, "\n")[1:]

	for i, spec := range strings.Split(strings.TrimSpace(specs), "\n") {
		if i >= len(rows) {
			t.Fatalf("conflictSpecs has %d entries but assignNames has %d rows",
				i+1, len(rows))
		}
		c := rows[i]
		for _, col := range []string{"tenant", "slug", "name"} {
			// A plain key column is equal on both sides — that is why the row
			// conflicted — so assigning it from EXCLUDED is noise.
			if strings.Contains(spec, `\"`+col+`\"`) && strings.Contains(c, `"`+col+`"`) {
				t.Errorf("target %d (%s) assigns its own key column %s:\n%s", i, spec, col, c)
			}
		}
		// An EXPRESSION key is different: lower(email) matching does not make
		// the emails equal, and the caller's spelling is what they meant to
		// store. Its column has to stay assignable.
		if strings.Contains(spec, "lower(email)") && !strings.Contains(c, `"email"`) {
			t.Errorf("target %d is keyed on lower(email) but cannot assign email:\n%s", i, c)
		}
	}
}

// between returns the text between two markers, failing the test rather than
// panicking when the generated shape has moved.
func between(t *testing.T, src, open, close string) string {
	t.Helper()
	i := strings.Index(src, open)
	if i < 0 {
		t.Fatalf("generated code has no %q", open)
	}
	rest := src[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("no %q after %q", close, open)
	}
	return rest[:j]
}

// wcBucket is the counter an adopter upserts into: one row per host, hits and
// cpu seconds that accumulate across writers.
type wcBucket struct {
	storm.Model
	Host       string
	Hits       int64
	CPUSeconds float64
	Note       string
	Label      *string
}

func (b *wcBucket) Schema(t *storm.Table) { t.Unique(&b.Host) }

// Add…OnConflict is generated for the NOT NULL numeric columns (named, like
// every generated identifier, from the column: cpu_seconds is CpuSeconds) an upsert can
// write, and for nothing else: a nullable column would add into NULL and get
// NULL back, and a text column has no sum.
func TestUpsert_AddOnConflictIsTypedPerColumn(t *testing.T) {
	src := find(t, gen(t, codegen.PackageOptions{}, &wcBucket{}), "wcbucket")
	for _, want := range []string{
		"func (n *Ins) AddHitsOnConflict(v int64)",
		"func (n *Ins) AddCpuSecondsOnConflict(v float64)",
		`"\"hits\" = \"wc_buckets\".\"hits\" + EXCLUDED.\"hits\""`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, refused := range []string{"AddNoteOnConflict", "AddLabelOnConflict", "AddHostOnConflict"} {
		if strings.Contains(src, refused) {
			t.Errorf("%s was generated; the column cannot accumulate", refused)
		}
	}
}

// TestSetBasedWritesAgainstPostgres runs Add…OnConflict, UpdateAll and
// DeleteAll against a real server, from a generated package written into the
// module — the claims are about what PostgreSQL does with the statements, and
// a source-text test would pass a statement PostgreSQL rejects.
func TestSetBasedWritesAgainstPostgres(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset")
	}
	s, err := storm.Build(&wcBucket{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	base := "wclive" + strconv.Itoa(os.Getpid())
	dir := filepath.Join(root, "internal", base)
	t.Cleanup(func() { os.RemoveAll(dir) })
	files, err := codegen.Package(s, codegen.PackageOptions{Dir: dir, Import: "github.com/gsoultan/storm"})
	if err != nil {
		t.Fatal(err)
	}
	var pkgDir string
	for rel, src := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, src, 0o644); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(rel, ".gen.go") && strings.Contains(rel, "/") {
			pkgDir = filepath.Dir(full)
		}
	}
	if pkgDir == "" {
		t.Fatal("no per-table package was generated")
	}
	pkg := filepath.Base(pkgDir)
	src := strings.ReplaceAll(setBasedLiveSrc, "PKG", pkg)
	src = strings.ReplaceAll(src, "IMPORTPATH", "github.com/gsoultan/storm/internal/"+base+"/"+pkg)
	if err := os.WriteFile(filepath.Join(pkgDir, "live_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-count=1", "-v", "./internal/"+base+"/"+pkg+"/")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "STORM_DSN="+os.Getenv("STORM_DSN"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("set-based writes do not behave against a real database:\n%s", out)
	}
	for _, name := range []string{
		"TestAddOnConflictAccumulates",
		"TestAddOnConflictSurvivesConcurrentWriters",
		"TestAddOnConflictInABatch",
		"TestSetAfterAddOverwrites",
		"TestUpdateAllWritesOnlyMatchingRows",
		"TestDeleteAllRemovesOnlyMatchingRows",
		"TestWriteAllRefusals",
	} {
		if !strings.Contains(string(out), "--- PASS: "+name) {
			t.Errorf("%s did not run:\n%s", name, out)
		}
	}
}

const setBasedLiveSrc = `package PKG_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	wc "IMPORTPATH"
	"github.com/jackc/pgx/v5/pgxpool"
)

type wcBucket struct {
	storm.Model
	Host       string
	Hits       int64
	CPUSeconds float64
	Note       string
	Label      *string
}

func (b *wcBucket) Schema(t *storm.Table) { t.Unique(&b.Host) }

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
	s, err := storm.Build(&wcBucket{})
	must(err)
	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_setbased_live"
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

func upsert(t *testing.T, host string, hits int64, cpu float64) wc.Row {
	t.Helper()
	n := wc.Create()
	n.SetHost(host)
	n.SetNote("n")
	n.AddHitsOnConflict(hits)
	n.AddCpuSecondsOnConflict(cpu)
	r, err := n.OnConflictHost().Insert(context.Background(), ex)
	if err != nil {
		t.Fatalf("upsert %s: %v", host, err)
	}
	return r
}

func hits(t *testing.T, host string) int64 {
	t.Helper()
	r, ok, err := wc.New().HostEq(host).One(context.Background(), ex)
	if err != nil || !ok {
		t.Fatalf("read %s: ok=%v err=%v", host, ok, err)
	}
	return r.Hits
}

// The first upsert inserts the value; every later one adds to what is there,
// and the row handed back is the row that now exists.
func TestAddOnConflictAccumulates(t *testing.T) {
	if r := upsert(t, "acc", 5, 1.5); r.Hits != 5 || r.CpuSeconds != 1.5 {
		t.Fatalf("first upsert: %+v", r)
	}
	r := upsert(t, "acc", 3, 0.25)
	if r.Hits != 8 || r.CpuSeconds != 1.75 {
		t.Fatalf("second upsert returned hits=%d cpu=%v; want 8 and 1.75", r.Hits, r.CpuSeconds)
	}
}

// The reason it is a database-side sum: N writers each adding one must end at
// N. Adding in Go and upserting the total loses writes under exactly this.
func TestAddOnConflictSurvivesConcurrentWriters(t *testing.T) {
	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := wc.Create()
			n.SetHost("race")
			n.SetNote("n")
			n.SetCpuSeconds(0)
			n.AddHitsOnConflict(1)
			if _, err := n.OnConflictHost().Insert(context.Background(), ex); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := hits(t, "race"); got != writers {
		t.Fatalf("%d concurrent +1 upserts left hits at %d", writers, got)
	}
}

// The batch form carries the accumulate mask too: Op() is the bulk upsert.
func TestAddOnConflictInABatch(t *testing.T) {
	ops := make([]runtime.BatchOp, 0, 4)
	for range 4 {
		n := wc.Create()
		n.SetHost("batch")
		n.SetNote("n")
		n.SetCpuSeconds(0)
		n.AddHitsOnConflict(10)
		op, err := n.OnConflictHost().Op()
		if err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	if err := ex.Batch(context.Background(), ops, nil); err != nil {
		t.Fatal(err)
	}
	if got := hits(t, "batch"); got != 40 {
		t.Fatalf("four batched +10 upserts left hits at %d", got)
	}
}

// The two setters clear each other, like Mut's: the last call decides.
func TestSetAfterAddOverwrites(t *testing.T) {
	upsert(t, "over", 7, 0)
	n := wc.Create()
	n.SetHost("over")
	n.SetNote("n")
	n.SetCpuSeconds(0)
	n.AddHitsOnConflict(100)
	n.SetHits(2)
	if _, err := n.OnConflictHost().Insert(context.Background(), ex); err != nil {
		t.Fatal(err)
	}
	if got := hits(t, "over"); got != 2 {
		t.Fatalf("SetHits after AddHitsOnConflict left %d; want the overwrite, 2", got)
	}
}

func TestUpdateAllWritesOnlyMatchingRows(t *testing.T) {
	ctx := context.Background()
	for _, h := range []string{"ua-1", "ua-2", "ua-3"} {
		upsert(t, h, 1, 0)
	}
	var m wc.Mut
	m.SetNote("bulk")
	m.IncHits()
	n, err := wc.New().Where(wc.Host.In("ua-1", "ua-2")).UpdateAll(ctx, ex, m)
	if err != nil {
		t.Fatalf("UpdateAll: %v", err)
	}
	if n != 2 {
		t.Fatalf("UpdateAll reported %d rows; two matched", n)
	}
	rows, err := wc.New().Where(wc.Host.In("ua-1", "ua-2", "ua-3")).Order(wc.Host.Asc()).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	for i, r := range rows {
		matched := i < 2
		if matched && (r.Note != "bulk" || r.Hits != 2) {
			t.Errorf("%s was matched but reads %+v", r.Host, r)
		}
		if !matched && (r.Note != "n" || r.Hits != 1) {
			t.Errorf("%s was NOT matched but was written: %+v", r.Host, r)
		}
	}
	// Nothing assigned issues nothing.
	if n, err := wc.New().HostEq("ua-3").UpdateAll(ctx, ex, wc.Mut{}); n != 0 || err != nil {
		t.Errorf("an empty Mut wrote %d rows (%v)", n, err)
	}
}

func TestDeleteAllRemovesOnlyMatchingRows(t *testing.T) {
	ctx := context.Background()
	for _, h := range []string{"da-1", "da-2", "da-keep"} {
		upsert(t, h, 1, 0)
	}
	n, err := wc.New().Where(wc.Host.Like("da-_")).DeleteAll(ctx, ex)
	if err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if n != 2 {
		t.Fatalf("DeleteAll reported %d rows; two matched", n)
	}
	left, err := wc.New().Where(wc.Host.Like("da-%")).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("%d rows left; only da-keep should be", left)
	}
}

// The two shapes that read fine and write the wrong rows are refused.
func TestWriteAllRefusals(t *testing.T) {
	ctx := context.Background()
	upsert(t, "refuse", 1, 0)
	if _, err := wc.New().DeleteAll(ctx, ex); !errors.Is(err, runtime.ErrUnfilteredWrite) {
		t.Errorf("DeleteAll with no predicate: %v", err)
	}
	var m wc.Mut
	m.SetNote("x")
	if _, err := wc.New().UpdateAll(ctx, ex, m); !errors.Is(err, runtime.ErrUnfilteredWrite) {
		t.Errorf("UpdateAll with no predicate: %v", err)
	}
	q := wc.New().HostEq("refuse")
	for name, bad := range map[string]wc.Query{
		"Limit": q.Limit(1), "Offset": q.Offset(1), "ForUpdate": q.ForUpdate(),
	} {
		if _, err := bad.DeleteAll(ctx, ex); !errors.Is(err, runtime.ErrPagedWrite) {
			t.Errorf("DeleteAll with %s: %v", name, err)
		}
		if _, err := bad.UpdateAll(ctx, ex, m); !errors.Is(err, runtime.ErrPagedWrite) {
			t.Errorf("UpdateAll with %s: %v", name, err)
		}
	}
	if got := hits(t, "refuse"); got != 1 {
		t.Errorf("a refused write still wrote: hits=%d", got)
	}
}
`
