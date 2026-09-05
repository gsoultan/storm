package planspike_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store/org"
	"github.com/gsoultan/storm/internal/planspike/store/user"
	"github.com/gsoultan/storm/runtime"
)

// A UNIQUE constraint cannot hold an expression, so t.Unique(storm.Lower(&u.Email))
// becomes a unique INDEX — and until conflict targets came from indexes too,
// case-insensitive email, the canonical upsert target, had no OnConflict
// method at all and no message saying why.
func TestUpsert_OnAnExpressionUniqueIndex(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	org := upsertOrg(t, ctx, ex, "expr")

	n := user.Create()
	n.SetEmail("Casing@upsert-expr.invalid")
	n.SetName("First")
	n.SetStatus("active")
	n.SetOrgID(org)
	first, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}

	// A different spelling of the same address: lower(email) collides.
	up := user.Create()
	up.SetEmail("CASING@UPSERT-EXPR.INVALID")
	up.SetName("Second")
	up.SetOrgID(org)
	up.OnConflictLowerEmail()

	got, err := up.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID {
		t.Fatalf("the upsert inserted a second row rather than updating the first")
	}
	if got.Name != "Second" {
		t.Errorf("Name = %q, want Second", got.Name)
	}
	// The mask is the whole correctness question: an upsert that assigned
	// every column would revert Status to its default on a row it never
	// meant to touch.
	if got.Status != "active" {
		t.Errorf("Status = %q — the upsert reverted a column it was never given", got.Status)
	}
	// And the expression key ITSELF is overwritten, unlike a plain key
	// column: lower(email) matching does not mean the emails are equal.
	if got.Email != "CASING@UPSERT-EXPR.INVALID" {
		t.Errorf("Email = %q — an expression key must be assignable, or the "+
			"casing the caller sent is silently discarded", got.Email)
	}
}

// The idempotent insert. Before DoNothing, DO NOTHING was reachable only by
// accident — when the caller happened to assign no updatable column.
func TestUpsert_DoNothingIsIdempotent(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	org := upsertOrg(t, ctx, ex, "idem")

	n := user.Create()
	n.SetEmail("idem@upsert-idem.invalid")
	n.SetName("Original")
	n.SetStatus("active")
	n.SetOrgID(org)
	first, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		make func() *user.Ins
	}{
		{"bare — any unique index", func() *user.Ins {
			m := user.Create()
			m.DoNothing()
			return &m
		}},
		{"targeted", func() *user.Ins {
			m := user.Create()
			m.OnConflictLowerEmail().DoNothing()
			return &m
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := c.make()
			m.SetEmail("idem@upsert-idem.invalid")
			m.SetName("Ignored")
			m.SetStatus("suspended")
			m.SetOrgID(org)

			_, err := m.Insert(ctx, ex)
			// DO NOTHING suppresses RETURNING, so no row is the SUCCESS case.
			// It has to be distinguishable from a real absence, or a caller
			// treats an idempotent insert as a failure and retries forever.
			if !errors.Is(err, runtime.ErrConflict) {
				t.Fatalf("err = %v, want runtime.ErrConflict", err)
			}
			after, ok, err := user.New().IDEq(first.ID).One(ctx, ex)
			if err != nil || !ok {
				t.Fatalf("re-read: %v", err)
			}
			if after.Name != "Original" || after.Status != "active" {
				t.Errorf("DoNothing wrote: Name=%q Status=%q", after.Name, after.Status)
			}
		})
	}
}

// The bulk upsert. InsertOp writes every column of a Row and cannot conflict;
// ingesting a batch that should overwrite what is already there needed the
// builder's mask and conflict, and could otherwise only be done one round
// trip at a time.
func TestUpsert_InBatch(t *testing.T) {
	ctx := context.Background()
	ex, counter := db(t)
	org := upsertOrg(t, ctx, ex, "batch")

	n := user.Create()
	n.SetEmail("one@upsert-batch.invalid")
	n.SetName("Before")
	n.SetStatus("active")
	n.SetOrgID(org)
	first, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}

	ops := make([]runtime.BatchOp, 0, 3)
	for _, c := range []struct{ email, name string }{
		{"one@upsert-batch.invalid", "After"}, // conflicts: updates
		{"two@upsert-batch.invalid", "New2"},  // does not: inserts
		{"three@upsert-batch.invalid", "New3"},
	} {
		m := user.Create()
		m.SetEmail(c.email)
		m.SetName(c.name)
		m.SetOrgID(org)
		m.OnConflictLowerEmail()
		op, err := m.Op()
		if err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}

	counter.Reset()
	if err := ex.Batch(ctx, ops, nil); err != nil {
		t.Fatal(err)
	}
	if got := counter.RoundTrips(); got != 1 {
		t.Errorf("the upsert batch cost %d round trips, want 1", got)
	}

	after, ok, err := user.New().IDEq(first.ID).One(ctx, ex)
	if err != nil || !ok {
		t.Fatalf("re-read: %v", err)
	}
	if after.Name != "After" {
		t.Errorf("Name = %q, want After — the batch did not upsert", after.Name)
	}
	if after.Status != "active" {
		t.Errorf("Status = %q — the batched upsert reverted a column it was never given", after.Status)
	}
	for _, e := range []string{"two@upsert-batch.invalid", "three@upsert-batch.invalid"} {
		if n, err := user.New().EmailEq(e).Count(ctx, ex); err != nil || n != 1 {
			t.Errorf("%s: count = %d, err = %v; a non-conflicting row must insert", e, n, err)
		}
	}
}

// Every row of a batch that assigns the same columns shares one statement,
// and a batch is only worth having if that holds.
func TestUpsert_BatchSharesOneStatement(t *testing.T) {
	org := [16]byte{1}
	sql := map[string]bool{}
	for i := 0; i < 5; i++ {
		m := user.Create()
		m.SetEmail("x")
		m.SetName("y")
		m.SetOrgID(org)
		m.OnConflictLowerEmail()
		op, err := m.Op()
		if err != nil {
			t.Fatal(err)
		}
		sql[op.SQL] = true
		if strings.Contains(op.SQL, "RETURNING") {
			t.Errorf("a batched insert asks for rows back:\n%s", op.SQL)
		}
		if !strings.Contains(op.SQL, "ON CONFLICT") {
			t.Errorf("the conflict clause did not survive into the batch op:\n%s", op.SQL)
		}
	}
	if len(sql) != 1 {
		t.Errorf("five identical upserts compiled %d statements", len(sql))
	}
}

// upsertOrg gives each test an org of its own.
//
// Borrowing a seeded one and adding users to it breaks every test that counts
// them — TestPlan_TwoRoundTrips asserts 500 per org — and only under
// -shuffle=on, when the order happens to put the writer first. The fixture is
// shared in BOTH directions, which is the mistake m2mAuthor above already
// documents and this test made again.
func upsertOrg(t *testing.T, ctx context.Context, ex runtime.Executor, name string) [16]byte {
	t.Helper()
	no := org.Create()
	no.SetName("upsert-" + name)
	o, err := no.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		rows, err := user.New().OrgIDEq(o.ID).All(ctx, ex, nil)
		if err != nil {
			return
		}
		for _, r := range rows {
			_ = user.Delete(ctx, ex, r.ID)
		}
		_ = org.Delete(ctx, ex, o.ID)
	})
	return o.ID
}
