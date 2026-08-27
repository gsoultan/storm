package planspike_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store/user"
	"github.com/gsoultan/storm/runtime"
)

func anOrg(t *testing.T) [16]byte {
	t.Helper()
	ex, _ := db(t)
	orgs, err := orgRows(context.Background(), ex)
	if err != nil || len(orgs) == 0 {
		t.Fatalf("need a seeded org: %v", err)
	}
	return orgs[0]
}

// Insert must read back what the database computed. Learning a generated id
// with a second SELECT races every other writer.
func TestInsert_FillsDatabaseDefaults(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	n := user.Create()
	n.SetEmail("insert@example.com")
	n.SetName("Inserted")
	n.SetStatus("pending")
	n.SetOrgID(anOrg(t))

	r, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	// id and created_at were never assigned, so the database supplied them.
	// That is the whole reason absence is tracked by a mask instead of being
	// inferred from a zero value.
	if r.ID == ([16]byte{}) {
		t.Error("ID is zero — DEFAULT gen_random_uuid() did not fire, or RETURNING did not come back")
	}
	if r.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero — the now() default did not come back")
	}
	if r.Email != "insert@example.com" {
		t.Errorf("Email = %q after insert", r.Email)
	}
}

// An insert naming no columns would take every default — almost never what the
// caller meant, and never what they said.
func TestInsert_NothingAssignedIsAnError(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	n := user.Create()
	if _, err := n.Insert(ctx, ex); !errors.Is(err, runtime.ErrNothingAssigned) {
		t.Errorf("empty insert returned %v, want ErrNothingAssigned", err)
	}
}

// A zero value that was explicitly assigned must be written, not treated as
// absent. This is the bug the mask exists to prevent.
func TestInsert_ExplicitZeroIsWritten(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	n := user.Create()
	n.SetEmail("zero@example.com")
	n.SetName("") // deliberately empty, not absent
	n.SetStatus("pending")
	n.SetOrgID(anOrg(t))
	n.SetVersion(0) // deliberately zero, not absent

	r, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	if r.Name != "" {
		t.Errorf("Name = %q, want the empty string that was assigned", r.Name)
	}
}

// An UPDATE writes the columns that were assigned and no others: a
// read-modify-write must not clobber a column it never looked at.
func TestUpdate_WritesOnlyDirtyColumns(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := mustCreate(t, ctx, ex, "dirty@example.com", "Before")
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	m := user.Mutate(r)
	m.SetName("After")
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}

	got, ok, err := user.New().Where(user.ID.Eq(r.ID)).One(ctx, ex)
	if err != nil || !ok {
		t.Fatalf("re-read: %v ok=%v", err, ok)
	}
	if got.Name != "After" {
		t.Errorf("Name = %q, want After", got.Name)
	}
	if got.Email != "dirty@example.com" {
		t.Errorf("Email = %q — an untouched column was rewritten", got.Email)
	}
	if got.Version != r.Version+1 {
		t.Errorf("Version = %d, want %d — the update did not bump it", got.Version, r.Version+1)
	}
}

// Assigning nothing must issue no statement. A caller looping over
// possibly-changed fields should not have to special-case the empty case, and
// an UPDATE with an empty SET list is not valid SQL.
func TestUpdate_NoDirtyColumnsIsNoStatement(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	count.Reset()

	m := user.Mutate(user.Row{})
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if n := count.RoundTrips(); n != 0 {
		t.Errorf("%d round trips for an empty update, want 0", n)
	}
}

// THE GATE: the dirty set costs nothing to compute.
func TestUpdate_DirtySetIsZeroAlloc(t *testing.T) {
	r := user.Row{Email: "a@b.com", Name: "n", Status: "pending"}
	if n := testing.AllocsPerRun(1000, func() {
		m := user.Mutate(r)
		m.SetName("x")
		m.SetEmail("y@z.com")
		m.SetStatus("active")
		if m.Dirty() == 0 {
			t.Fatal("mask not set")
		}
	}); n != 0 {
		t.Errorf("dirty-set computation allocates %v times per run, want 0", n)
	}
}

// One statement per distinct mask, and only one — the same bargain the read
// path makes, keyed by a bitmask instead of a token stream.
func TestUpdate_OneStatementPerMask(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := mustCreate(t, ctx, ex, "mask@example.com", "M")
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	// Deltas, not absolutes: another test in this package may already have
	// compiled the {Name} mask, and a cache that works makes that invisible.
	before := user.Masks()
	for i := 0; i < 20; i++ { // the same mask, twenty times
		m := user.Mutate(r)
		m.SetName("M")
		if err := m.Update(ctx, ex); err != nil {
			t.Fatal(err)
		}
		r = m.Row()
	}
	afterSame := user.Masks()
	if n := afterSame - before; n > 1 {
		t.Errorf("twenty updates of one shape compiled %d statements, want at most 1", n)
	}

	m := user.Mutate(r) // a mask nothing has used
	m.SetName("M2")
	m.SetEmail("mask2@example.com")
	m.SetStatus("active")
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if n := user.Masks() - afterSame; n != 1 {
		t.Errorf("a new mask compiled %d statements, want exactly 1", n)
	}
}

// THE GATE: a concurrent update proves the version column rejects the writer
// that computed its change from a value that is no longer true.
func TestUpdate_OptimisticLockRejectsStaleWriter(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := mustCreate(t, ctx, ex, "lock@example.com", "Start")
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	// Both writers read the same version, as two request handlers would.
	a, b := user.Mutate(r), user.Mutate(r)
	a.SetName("A")
	b.SetName("B")

	if err := a.Update(ctx, ex); err != nil {
		t.Fatalf("first writer should win: %v", err)
	}
	err := b.Update(ctx, ex)
	if !errors.Is(err, runtime.ErrStaleWrite) {
		t.Fatalf("second writer got %v, want ErrStaleWrite — it computed its change from a stale read", err)
	}

	got, _, _ := user.New().Where(user.ID.Eq(r.ID)).One(ctx, ex)
	if got.Name != "A" {
		t.Errorf("Name = %q — the loser's write landed anyway", got.Name)
	}
}

// Under real contention exactly one writer per version may win.
func TestUpdate_OptimisticLockUnderContention(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := mustCreate(t, ctx, ex, "race@example.com", "Start")
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	const writers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	var won, stale int
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := user.Mutate(r) // every one read version 0
			m.SetName("w")
			err := m.Update(ctx, ex)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, runtime.ErrStaleWrite):
				stale++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("%d writers won, want exactly 1", won)
	}
	if stale != writers-1 {
		t.Errorf("%d stale, want %d", stale, writers-1)
	}
}

// Deleting a row that is already gone is an error, not success: a caller
// deleting something that is not there usually has a bug.
func TestDelete_MissingRowIsAnError(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	if err := user.Delete(ctx, ex, [16]byte{9, 9, 9}); !errors.Is(err, runtime.ErrNoRow) {
		t.Errorf("deleting an absent row returned %v, want ErrNoRow", err)
	}
}

// The generated SQL must name only the assigned columns. This asserts the text
// because "writes only dirty columns" is a claim about the statement, and a
// behavioural test would still pass if the statement wrote every column to its
// existing value.
func TestUpdate_SQLNamesOnlyDirtyColumns(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	rec := &recorder{Executor: ex}

	r := user.Row{ID: [16]byte{1}, Version: 0}
	m := user.Mutate(r)
	m.SetName("only-this")
	_ = m.Update(ctx, rec)

	if !strings.Contains(rec.sql, `"name" = $1`) {
		t.Errorf("SET list does not assign name:\n%s", rec.sql)
	}
	if strings.Contains(rec.sql, `"email"`) {
		t.Errorf("SET list assigns email, which was never set:\n%s", rec.sql)
	}
	if !strings.Contains(rec.sql, `"version" = "version" + 1`) {
		t.Errorf("version is not incremented from its own value:\n%s", rec.sql)
	}
	if !strings.Contains(rec.sql, `"version" = $`) {
		t.Errorf("WHERE clause does not carry the optimistic lock:\n%s", rec.sql)
	}
}

type recorder struct {
	runtime.Executor
	sql string
}

func (r *recorder) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	r.sql = sql
	return r.Executor.Exec(ctx, sql, args)
}

func mustCreate(t *testing.T, ctx context.Context, ex runtime.Executor, email, name string) user.Row {
	t.Helper()
	n := user.Create()
	n.SetEmail(email)
	n.SetName(name)
	n.SetStatus("pending")
	n.SetOrgID(anOrg(t))
	r, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
