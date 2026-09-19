package planspike_test

import (
	"context"
	"sync"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store/post"
	"github.com/gsoultan/storm/internal/planspike/store/user"
)

// aUser inserts one row and removes it afterwards.
func aUser(t *testing.T, email string) user.Row {
	t.Helper()
	ctx := context.Background()
	ex, _ := db(t)

	n := user.Create()
	n.SetEmail(email)
	n.SetName("Expr")
	n.SetStatus("pending")
	n.SetOrgID(anOrg(t))
	n.SetAge(30)

	r, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })
	return r
}

// aPost inserts one post and removes it afterwards.
func aPost(t *testing.T) post.Row {
	t.Helper()
	ctx := context.Background()
	ex, _ := db(t)

	n := post.Create()
	n.SetTitle("Expr")
	n.SetBody("body")
	n.SetAuthorID(aUser(t, "postauthor@example.com").ID)

	r, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = post.Delete(ctx, ex, r.ID) })
	return r
}

func reread(t *testing.T, id [16]byte) user.Row {
	t.Helper()
	ex, _ := db(t)
	r, ok, err := user.New().Where(user.ID.Eq(id)).One(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the row is gone")
	}
	return r
}

// A server-side assignment must be read back, or the staged row silently
// disagrees with the database it was just written to.
func TestSetNow_RefreshesTheStagedRow(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := aUser(t, "setnow@example.com")
	m := user.Mutate(r)
	m.SetName("Renamed")
	m.SetUpdatedAtNow()

	if m.Expr() == 0 {
		t.Fatal("SetUpdatedAtNow set no expression bit")
	}
	if m.Dirty()&m.Expr() != 0 {
		t.Fatal("a column is staged as both a value and an expression")
	}

	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}

	got := m.Row().UpdatedAt
	if !got.After(r.UpdatedAt) {
		t.Fatalf("staged UpdatedAt did not move: %v then %v", r.UpdatedAt, got)
	}
	// The staged value must be the one the DATABASE holds, not merely a
	// newer one — a refresh that invented a timestamp would pass the check
	// above and still be wrong.
	if db := reread(t, r.ID).UpdatedAt; !db.Equal(got) {
		t.Fatalf("staged %v, database %v", got, db)
	}
	if m.Row().Name != "Renamed" {
		t.Fatalf("the bound value was lost in the refresh: %q", m.Row().Name)
	}
	if m.Dirty() != 0 || m.Expr() != 0 {
		t.Fatalf("masks not cleared after Update: dirty=%d expr=%d", m.Dirty(), m.Expr())
	}
}

// THE REASON Inc exists. Every increment must land even when the callers all
// started from the same stale row — computed in Go this is a read-modify-write
// and the concurrent ones overwrite each other with the same number.
//
// posts carries no version column on purpose: with one, the optimistic lock
// would reject the stale writers and the counter would be correct for a
// different reason, proving nothing about where the addition happened.
func TestInc_EveryConcurrentIncrementLands(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	const writers = 12
	r := aPost(t)
	start := r.ViewCount

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := post.Mutate(r) // every writer holds the SAME stale row
			m.IncViewCount()
			if err := m.Update(ctx, ex); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	got, ok, err := post.New().Where(post.ID.Eq(r.ID)).One(ctx, ex)
	if err != nil || !ok {
		t.Fatalf("re-read: %v ok=%v", err, ok)
	}
	if want := start + writers; got.ViewCount != want {
		t.Fatalf("view_count is %d, want %d — %d increments were issued from one stale row",
			got.ViewCount, want, writers)
	}
}

// The two setters are exclusive: a column is written one way or the other,
// and the last call decides. Without the clearing, both would reach the SET
// list and the column would be assigned twice in one statement.
func TestValueAndExpressionAreExclusive(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := aPost(t)

	m := post.Mutate(r)
	m.SetViewCount(50)
	m.IncViewCount() // the expression is last, so it wins
	if m.Dirty() != 0 {
		t.Fatalf("the bound value survived the expression: dirty=%d", m.Dirty())
	}
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if got := m.Row().ViewCount; got != r.ViewCount+1 {
		t.Fatalf("view_count is %d, want %d", got, r.ViewCount+1)
	}

	m2 := post.Mutate(m.Row())
	m2.IncViewCount()
	m2.SetViewCount(50) // the value is last, so it wins
	if m2.Expr() != 0 {
		t.Fatalf("the expression survived the bound value: expr=%d", m2.Expr())
	}
	if err := m2.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	got, _, err := post.New().Where(post.ID.Eq(r.ID)).One(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if got.ViewCount != 50 {
		t.Fatalf("view_count is %d, want 50", got.ViewCount)
	}
}

// An UPDATE that assigns only an expression still has a SET list, so it must
// issue a statement rather than being mistaken for "nothing to do".
func TestExpressionAloneStillWrites(t *testing.T) {
	ctx := context.Background()
	ex, c := db(t)

	r := aUser(t, "exproneonly@example.com")
	before := c.RoundTrips()

	m := user.Mutate(r)
	m.SetUpdatedAtNow()
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if after := c.RoundTrips(); after == before {
		t.Fatal("no statement was issued for an expression-only update")
	}
	if got := reread(t, r.ID).UpdatedAt; !got.After(r.UpdatedAt) {
		t.Fatalf("the database did not move UpdatedAt: %v then %v", r.UpdatedAt, got)
	}
}

// The shape this whole feature exists for: stamp a column with the database's
// clock, addressed by key, without reading the row first. One round trip.
func TestMutateKey_StampsWithoutReadingFirst(t *testing.T) {
	ctx := context.Background()
	ex, c := db(t)

	r := aPost(t)
	c.Reset()

	m := post.MutateKey(r.ID)
	m.SetPublishedAtNow()
	m.IncViewCount()
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if n := c.RoundTrips(); n != 1 {
		t.Fatalf("%d round trips, want 1 — the point is not reading the row first", n)
	}

	// The refresh makes the staged row whole even though it started as an
	// address: the columns nobody assigned come back with the values the
	// database holds, not the zeroes MutateKey put there.
	got := m.Row()
	if !got.PublishedAt.Valid {
		t.Fatal("published_at was not stamped")
	}
	if got.ViewCount != r.ViewCount+1 {
		t.Fatalf("view_count is %d, want %d", got.ViewCount, r.ViewCount+1)
	}
	if got.Title != r.Title {
		t.Fatalf("title came back %q, want %q — an unassigned column was clobbered", got.Title, r.Title)
	}
}
