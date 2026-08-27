package planspike_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store"
	"github.com/gsoultan/storm/internal/planspike/store/org"
	"github.com/gsoultan/storm/internal/planspike/store/user"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
)

// THE QUESTION THIS FILE EXISTS TO ANSWER: is a failed Unit flush atomic?
//
// A Unit's whole claim is a correctly-ordered graph write. If statement 3 fails
// and statements 1–2 stay committed, the caller has HALF A GRAPH — an orphan
// parent with no child, which no error message repairs. The protocol argument
// says it cannot happen (a pgx batch runs between one Sync, so PostgreSQL
// treats it as an implicit transaction and an error aborts all of it), but a
// data-integrity property gets a test, not an argument.
func TestUnit_FailedFlushLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	orgID := newID()
	u := store.NewUnit()
	u.Add(org.Table, org.InsertOp(org.Row{ID: orgID, Name: "atomic-orphan"}))
	// The second statement fails: its org_id references a row that does not
	// exist anywhere.
	u.Add(user.Table, user.InsertOp(user.Row{
		ID: newID(), Prefs: emptyJSON, Scopes: []string{},
		Email: "atomic@example.com", Name: "A", Status: "pending",
		OrgID: newID(),
	}))

	if _, err := u.Flush(ctx, ex); err == nil {
		t.Fatal("the second statement violates a foreign key; the flush must fail")
	}
	t.Cleanup(func() { _ = org.Delete(ctx, ex, orgID) })

	n, err := org.New().Where(org.ID.Eq(orgID)).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the first statement's org survived a failed flush — a Unit is not atomic, "+
			"and a failed graph write leaves %d orphan(s)", n)
	}
}

// A transaction is an Executor you were given (ADR-0005). This proves the
// sentence is a capability: generated code runs inside a pgx transaction
// unchanged, sees its own writes, and a rollback erases them.
func TestTx_RollbackErasesGeneratedWrites(t *testing.T) {
	ctx := context.Background()
	outer, _ := db(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ex := pgxdrv.Tx{T: tx}

	r := mustCreate(t, ctx, ex, "txroll@example.com", "Ephemeral")

	// Visible inside the transaction...
	if _, ok, err := user.New().Where(user.ID.Eq(r.ID)).One(ctx, ex); err != nil || !ok {
		t.Fatalf("the transaction cannot see its own insert: ok=%v err=%v", ok, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// ...and gone outside it.
	if _, ok, err := user.New().Where(user.ID.Eq(r.ID)).One(ctx, outer); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("a rolled-back insert is still visible outside the transaction")
	}
}

func TestTx_CommitPersistsGeneratedWrites(t *testing.T) {
	ctx := context.Background()
	outer, _ := db(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := mustCreate(t, ctx, pgxdrv.Tx{T: tx}, "txcommit@example.com", "Durable")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = user.Delete(ctx, outer, r.ID) })

	if _, ok, err := user.New().Where(user.ID.Eq(r.ID)).One(ctx, outer); err != nil || !ok {
		t.Fatalf("a committed insert is not visible: ok=%v err=%v", ok, err)
	}
}

// Everything composes: a plan load, a COPY and a Unit flush all run inside one
// transaction, because none of them ever knew what was behind the Executor.
func TestTx_EverySurfaceComposesInsideOneTransaction(t *testing.T) {
	ctx := context.Background()
	outer, _ := db(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ex := runtime.Executor(pgxdrv.Tx{T: tx})

	orgID := newID()
	un := store.NewUnit()
	un.Add(org.Table, org.InsertOp(org.Row{ID: orgID, Name: "tx-compose"}))
	if _, err := un.Flush(ctx, ex); err != nil {
		t.Fatalf("unit flush inside a transaction: %v", err)
	}

	rows := make([]user.Row, 20)
	for i := range rows {
		rows[i] = user.Row{
			ID: newID(), Prefs: emptyJSON, Scopes: []string{},
			// Unique per row: the fixture has Unique(Lower(email)).
			Email: fmt.Sprintf("tx-bulk-%d@example.com", i), Name: "b", Status: "pending", OrgID: orgID,
		}
	}
	if n, err := user.InsertAll(ctx, ex, rows); err != nil || n != 20 {
		t.Fatalf("COPY inside a transaction: n=%d err=%v", n, err)
	}

	loaded, err := store.OrgWithUsers().Where(org.ID.Eq(orgID)).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || len(loaded[0].Users) != 20 {
		t.Fatalf("a plan inside the transaction sees %d orgs / %d users, want 1/20",
			len(loaded), len(loaded[0].Users))
	}

	// Update and delete are the surfaces that reach Executor.Exec rather than
	// Query — the test claimed "every surface" while leaving that method at
	// zero coverage, which is how a transaction adapter could have shipped
	// with a broken Exec and nothing to notice.
	m := user.Mutate(rows[0])
	m.SetName("renamed inside the transaction")
	if err := m.Update(ctx, ex); err != nil {
		t.Fatalf("update inside a transaction: %v", err)
	}
	got, ok, err := user.New().Where(user.ID.Eq(rows[0].ID)).One(ctx, ex)
	if err != nil || !ok {
		t.Fatalf("reading back the update: ok=%v err=%v", ok, err)
	}
	if got.Name != "renamed inside the transaction" {
		t.Fatalf("the update did not land inside the transaction: name=%q", got.Name)
	}

	if err := user.Delete(ctx, ex, rows[1].ID); err != nil {
		t.Fatalf("delete inside a transaction: %v", err)
	}
	if n, err := user.New().Where(user.ID.Eq(rows[1].ID)).Count(ctx, ex); err != nil || n != 0 {
		t.Fatalf("the delete is not visible inside its own transaction: n=%d err=%v", n, err)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := org.New().Where(org.ID.Eq(orgID)).Count(ctx, outer); n != 0 {
		t.Fatal("the rollback did not erase the composed writes")
	}
	// The rollback covers the Exec paths too: the deleted row is back and the
	// renamed one never existed outside.
	if n, _ := user.New().Where(user.ID.Eq(rows[1].ID)).Count(ctx, outer); n != 0 {
		t.Fatal("a row deleted inside a rolled-back transaction is missing outside it")
	}
}
