package codegen_test

// liveTestSrc is the test written beside the generated package. PKG is the
// package name and IMPORTPATH its import path; both are substituted before it
// is written.
//
// It asserts behaviour, not text: a deleted row stops coming back from every
// read, the address it held becomes claimable again, a restore brings it back,
// and a hard delete is the only thing that actually removes it.
const liveTestSrc = `package PKG_test

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
	sd "IMPORTPATH"
	"github.com/jackc/pgx/v5/pgxpool"
)

type sdUser struct {
	storm.Model
	Email     string
	Name      string
	DeletedAt *time.Time
}

func (u *sdUser) Schema(t *storm.Table) {
	t.SoftDelete(&u.DeletedAt)
	t.Unique(&u.Email)
}

var (
	pool *pgxpool.Pool
	ex   runtime.Executor
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		// NOT a skip. The caller only runs this package when it has a DSN, so
		// a missing one here means the environment did not survive the exec —
		// and skipping would report "soft delete works" having tested nothing.
		fmt.Fprintln(os.Stderr, "STORM_DSN did not reach the generated package")
		os.Exit(1)
	}
	ctx := context.Background()
	s, err := storm.Build(&sdUser{})
	must(err)

	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)
	const ns = "storm_softdelete_live"
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

var nextID byte

// insert gives every row an explicit id: a masked insert sends the column it
// was given, so a zero-valued Row asks for the zero uuid every time and the
// second one collides on the primary key.
func insert(t *testing.T, email, name string) [16]byte {
	t.Helper()
	nextID++
	var id [16]byte
	id[0], id[15] = nextID, nextID
	r := &sd.Row{ID: id, Email: email, Name: name}
	if err := sd.Insert(context.Background(), ex, r); err != nil {
		t.Fatalf("insert %s: %v", email, err)
	}
	return r.ID
}

func count(t *testing.T) int64 {
	t.Helper()
	n, err := sd.New().Count(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The claim the whole feature rests on: after Delete the row is still in the
// table, and no generated read returns it.
func TestDeletedRowVanishesFromEveryRead(t *testing.T) {
	ctx := context.Background()
	id := insert(t, "vanish@example.com", "Vanish")

	before := count(t)
	if err := sd.Delete(ctx, ex, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if after := count(t); after != before-1 {
		t.Errorf("count is %d after deleting one of %d", after, before)
	}

	rows, err := sd.New().Where(sd.ID.Eq(id)).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a deleted row came back from a filtered read: %+v", rows)
	}
	ok, err := sd.New().Where(sd.ID.Eq(id)).Exists(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("Exists reports a deleted row as present")
	}

	// Still physically there — that is what makes it SOFT.
	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM sd_users WHERE id = $1 AND deleted_at IS NOT NULL", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the row was actually removed; this is a hard delete wearing a costume")
	}
}

// Deleting twice is not success. A caller deleting something already gone
// usually believes something untrue about it.
func TestDeletingTwiceReportsNoRow(t *testing.T) {
	ctx := context.Background()
	id := insert(t, "twice@example.com", "Twice")
	if err := sd.Delete(ctx, ex, id); err != nil {
		t.Fatal(err)
	}
	if err := sd.Delete(ctx, ex, id); !errors.Is(err, runtime.ErrNoRow) {
		t.Errorf("second delete returned %v, want runtime.ErrNoRow", err)
	}
}

// The second hazard from CONCEPT.md, made concrete: a deleted row keeps its
// key, so without the partial unique index this insert fails.
func TestTheAddressOfADeletedRowCanBeClaimedAgain(t *testing.T) {
	ctx := context.Background()
	const email = "reuse@example.com"
	id := insert(t, email, "First")
	if err := sd.Delete(ctx, ex, id); err != nil {
		t.Fatal(err)
	}
	second := insert(t, email, "Second")
	if second == id {
		t.Fatal("the same row came back")
	}
	rows, err := sd.New().Where(sd.Email.Eq(email)).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "Second" {
		t.Errorf("expected exactly the live row, got %+v", rows)
	}
}

// Several deleted rows may share the value: a partial unique index constrains
// only the live ones, which is what makes repeated signup-and-delete work.
func TestSeveralDeletedRowsMayShareAUniqueValue(t *testing.T) {
	ctx := context.Background()
	const email = "churn@example.com"
	for i := 0; i < 3; i++ {
		id := insert(t, email, "Churn")
		if err := sd.Delete(ctx, ex, id); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM sd_users WHERE email = $1 AND deleted_at IS NOT NULL", email).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("%d deleted rows hold the address, want 3", n)
	}
	// And a live one can still take it.
	insert(t, email, "Latest")
}

// Uniqueness must still MEAN something. Scoping it to the live rows is only
// correct if two live rows are still refused.
func TestTwoLiveRowsStillCannotShareAUniqueValue(t *testing.T) {
	const email = "dup@example.com"
	insert(t, email, "First")

	nextID++
	var id [16]byte
	id[0], id[15] = nextID, nextID
	err := sd.Insert(context.Background(), ex, &sd.Row{ID: id, Email: email, Name: "Second"})
	if err == nil {
		t.Fatal("two LIVE rows shared a unique value — uniqueness is gone, not scoped")
	}
}

func TestRestoreBringsTheRowBack(t *testing.T) {
	ctx := context.Background()
	id := insert(t, "restore@example.com", "Restore")
	if err := sd.Delete(ctx, ex, id); err != nil {
		t.Fatal(err)
	}
	if err := sd.Restore(ctx, ex, id); err != nil {
		t.Fatalf("restore: %v", err)
	}
	rows, err := sd.New().Where(sd.ID.Eq(id)).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatal("a restored row did not come back")
	}
	// Restoring one that was never deleted is a caller believing something
	// untrue, so it reports no row rather than succeeding.
	if err := sd.Restore(ctx, ex, id); !errors.Is(err, runtime.ErrNoRow) {
		t.Errorf("restoring a live row returned %v, want runtime.ErrNoRow", err)
	}
}

func TestHardDeleteActuallyRemoves(t *testing.T) {
	ctx := context.Background()
	id := insert(t, "hard@example.com", "Hard")
	if err := sd.HardDelete(ctx, ex, id); err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sd_users WHERE id = $1", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("HardDelete left the row behind")
	}
}
`
