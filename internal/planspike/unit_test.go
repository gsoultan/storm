package planspike_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gsoultan/raorm/internal/planspike/store"
	"github.com/gsoultan/raorm/internal/planspike/store/org"
	"github.com/gsoultan/raorm/internal/planspike/store/user"
	"github.com/gsoultan/raorm/runtime"
)

// THE GATE. A graph is staged in the WRONG order — the child first — and must
// still be written correctly, in ONE round trip, with foreign keys NOT
// deferred.
//
// Not deferring is the whole point. Postgres would forgive any order if every
// constraint were DEFERRABLE INITIALLY DEFERRED, and an ORM that relies on that
// has not solved ordering, it has outsourced it — to a mechanism that only
// works on constraints somebody remembered to declare, and that reports the
// failure at COMMIT naming a constraint rather than the write that caused it.
func TestUnit_OrdersAGraphWithConstraintsNotDeferred(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)

	assertNotDeferred(t, ctx, "users")

	orgID, userID := newID(), newID()
	u := store.NewUnit()

	// Child first. A naive flush in declaration order violates users.org_id.
	u.Add(user.Table, user.InsertOp(user.Row{
		ID:    userID,
		Prefs: emptyJSON, Scopes: []string{}, Email: "graph@example.com", Name: "Graph",
		Status: "pending", OrgID: orgID,
	}))
	u.Add(org.Table, org.InsertOp(org.Row{ID: orgID, Name: "graph-org"}))

	t.Cleanup(func() {
		_ = user.Delete(ctx, ex, userID)
		_ = org.Delete(ctx, ex, orgID)
	})

	count.Reset()
	affected, err := u.Flush(ctx, ex)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(affected) != 2 {
		t.Fatalf("got %d results, want 2", len(affected))
	}
	for i, n := range affected {
		if n != 1 {
			t.Errorf("statement %d affected %d rows, want 1", i, n)
		}
	}
	if rt := count.RoundTrips(); rt != 1 {
		t.Errorf("%d round trips, want exactly 1", rt)
	}

	got, ok, err := user.New().Where(user.ID.Eq(userID)).One(ctx, ex)
	if err != nil || !ok {
		t.Fatalf("re-read the child: %v ok=%v", err, ok)
	}
	if got.OrgID != orgID {
		t.Error("the child's foreign key does not point at the parent")
	}
}

// assertNotDeferred fails if the database would have forgiven a wrong order
// anyway — otherwise the gate above proves nothing.
func assertNotDeferred(t *testing.T, ctx context.Context, table string) {
	t.Helper()
	var deferrable, deferred bool
	err := pool.QueryRow(ctx, `
		SELECT bool_or(condeferrable), bool_or(condeferred)
		FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE c.contype = 'f' AND t.relname = $1 AND n.nspname = $2`,
		table, planspikeSchm).Scan(&deferrable, &deferred)
	if err != nil {
		t.Fatalf("checking constraint deferrability: %v", err)
	}
	if deferrable || deferred {
		t.Fatalf("%s has deferrable foreign keys — the ordering gate would pass without ordering anything", table)
	}
}

// Within one table the caller's order is the only information available, and
// reordering it would break a delete-then-insert of the same key.
func TestUnit_PreservesOrderWithinATable(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	r := mustCreate(t, ctx, ex, "reinsert@example.com", "Before")
	u := store.NewUnit()
	u.Add(user.Table, user.DeleteOp(r.ID))
	u.Add(user.Table, user.InsertOp(user.Row{
		ID:    r.ID,
		Prefs: emptyJSON, Scopes: []string{}, Email: "reinsert@example.com", Name: "After",
		Status: "pending", OrgID: r.OrgID,
	}))
	t.Cleanup(func() { _ = user.Delete(ctx, ex, r.ID) })

	if _, err := u.Flush(ctx, ex); err != nil {
		t.Fatalf("delete-then-insert of one key must survive ordering: %v", err)
	}
	got, ok, err := user.New().Where(user.ID.Eq(r.ID)).One(ctx, ex)
	if err != nil || !ok {
		t.Fatalf("re-read: %v ok=%v", err, ok)
	}
	if got.Name != "After" {
		t.Errorf("Name = %q, want After — the two statements were reordered", got.Name)
	}
}

// A table outside this context cannot be ordered against it, because the
// foreign-key graph does not span them. Guessing would be worse than saying so.
func TestUnit_UnknownTableIsAnError(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	u := store.NewUnit()
	u.Add("some_other_context", runtime.BatchOp{SQL: "SELECT 1"})
	if _, err := u.Flush(ctx, ex); !errors.Is(err, runtime.ErrUnknownTable) {
		t.Errorf("flushing an unknown table returned %v, want ErrUnknownTable", err)
	}
}

func TestUnit_EmptyIsNoRoundTrip(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	count.Reset()
	if _, err := store.NewUnit().Flush(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if rt := count.RoundTrips(); rt != 0 {
		t.Errorf("%d round trips for an empty unit, want 0", rt)
	}
}

// The generated order must actually satisfy the model's foreign keys.
func TestFlushOrder_ParentsRankBelowChildren(t *testing.T) {
	if store.FlushOrder[org.Table] >= store.FlushOrder[user.Table] {
		t.Errorf("orgs ranks %d and users ranks %d — a parent must rank strictly lower",
			store.FlushOrder[org.Table], store.FlushOrder[user.Table])
	}
}
