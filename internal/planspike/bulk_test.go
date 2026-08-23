package planspike_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/gsoultan/raorm/internal/planspike/store/user"
	"github.com/gsoultan/raorm/runtime"
)

func newID() [16]byte { return uuid.New() }

// THE GATE: 1,000 inserts are one COPY. Not one round trip per row, not a
// batched INSERT — one conversation with the server, because COPY is a
// different wire path rather than a faster loop.
func TestInsertAll_ThousandRowsIsOneRoundTrip(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	org := anOrg(t)

	const n = 1000
	rows := make([]user.Row, n)
	for i := range rows {
		rows[i] = user.Row{
			ID:     newID(),
			Email:  fmt.Sprintf("bulk%d@example.com", i),
			Name:   fmt.Sprintf("bulk %d", i),
			Status: "pending",
			OrgID:  org,
		}
	}
	t.Cleanup(func() {
		for _, r := range rows {
			_ = user.Delete(ctx, ex, r.ID)
		}
	})

	count.Reset()
	loaded, err := user.InsertAll(ctx, ex, rows)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != n {
		t.Errorf("loaded %d rows, want %d", loaded, n)
	}
	if rt := count.RoundTrips(); rt != 1 {
		t.Errorf("%d round trips for %d rows, want exactly 1", rt, n)
	}

	got, err := user.New().Where(user.Email.Like("bulk%")).Limit(n+1).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Errorf("re-read found %d rows, want %d", got, n)
	}
}

// An empty bulk load must not talk to the server at all.
func TestInsertAll_EmptyIsNoRoundTrip(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	count.Reset()
	if _, err := user.InsertAll(ctx, ex, nil); err != nil {
		t.Fatal(err)
	}
	if rt := count.RoundTrips(); rt != 0 {
		t.Errorf("%d round trips for an empty load, want 0", rt)
	}
}

// COPY must not allocate per row. The row source reuses one buffer, because the
// driver consumes each slice before asking for the next.
func TestInsertAll_RowSourceIsAllocationFlat(t *testing.T) {
	ctx := context.Background()
	org := anOrg(t)
	mk := func(n int) []user.Row {
		rows := make([]user.Row, n)
		for i := range rows {
			rows[i] = user.Row{ID: newID(), Email: "x", Name: "y", Status: "pending", OrgID: org}
		}
		return rows
	}
	measure := func(n int) float64 {
		rows := mk(n)
		var c countingCopy
		return testing.AllocsPerRun(20, func() {
			c.n = 0
			_, _ = user.InsertAll(ctx, &c, rows)
		})
	}
	// Ten times the rows must not cost ten times the allocations. Comparing two
	// sizes rather than asserting an absolute keeps the test honest about the
	// fixed cost of setting up a load.
	small, large := measure(10), measure(100)
	if large > small*2 {
		t.Errorf("allocations scale with row count: %v at 10 rows, %v at 100", small, large)
	}
}

// countingCopy drains a CopySource without a database, so the allocation
// measurement is of raorm and not of pgx's wire encoder.
type countingCopy struct {
	runtime.Executor
	n int64
}

func (c *countingCopy) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	for src.Next() {
		if len(src.Values()) != len(cols) {
			return c.n, fmt.Errorf("row has %d values for %d columns", len(src.Values()), len(cols))
		}
		c.n++
	}
	return c.n, src.Err()
}

// THE GATE: a thousand mixed statements are one round trip.
func TestBatch_MixedStatementsIsOneRoundTrip(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	org := anOrg(t)

	const n = 1000
	ops := make([]runtime.BatchOp, 0, n)
	ids := make([][16]byte, 0, n)
	for i := range n {
		id := newID()
		ids = append(ids, id)
		st := user.InsertOp(user.Row{
			ID:     id,
			Email:  fmt.Sprintf("batch%d@example.com", i),
			Name:   fmt.Sprintf("batch %d", i),
			Status: "pending",
			OrgID:  org,
		})
		ops = append(ops, st)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_ = user.Delete(ctx, ex, id)
		}
	})

	count.Reset()
	var affected int64
	err := ex.Batch(ctx, ops, func(i int, rows runtime.Rows, n int64, err error) error {
		if err != nil {
			return fmt.Errorf("op %d: %w", i, err)
		}
		affected += n
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if affected != n {
		t.Errorf("%d rows affected, want %d", affected, n)
	}
	if rt := count.RoundTrips(); rt != 1 {
		t.Errorf("%d round trips for %d statements, want exactly 1", rt, n)
	}
}

// An upsert must overwrite only the columns the caller assigned. Overwriting
// the rest would silently revert every column the caller did not mention.
func TestUpsert_OverwritesOnlyAssignedColumns(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	first := mustCreate(t, ctx, ex, "upsert@example.com", "Original")
	t.Cleanup(func() { _ = user.Delete(ctx, ex, first.ID) })

	// Give the row a value that the upsert will not mention, so "only assigned
	// columns" is actually observable.
	m := user.Mutate(first)
	m.SetStatus("active")
	if err := m.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}

	// ON CONFLICT still requires a valid INSERT tuple: Postgres evaluates the
	// row before it decides there is a conflict, so every NOT NULL column
	// without a default has to be supplied even though the insert will not
	// happen. Status is deliberately omitted.
	n := user.Create()
	n.SetID(first.ID) // same primary key: this must update, not fail
	n.SetEmail("upsert@example.com")
	n.SetName("Replaced")
	n.SetOrgID(first.OrgID)
	n.OnConflictID()

	got, err := n.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Replaced" {
		t.Errorf("Name = %q, want Replaced", got.Name)
	}
	if got.Status != "active" {
		t.Errorf("Status = %q — the upsert reverted a column it was never given", got.Status)
	}
}

// Without a conflict target a duplicate must fail. Silently updating a row you
// meant to create is a data-loss bug that looks like success.
func TestUpsert_WithoutTargetADuplicateIsAnError(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	first := mustCreate(t, ctx, ex, "dup@example.com", "First")
	t.Cleanup(func() { _ = user.Delete(ctx, ex, first.ID) })

	n := user.Create()
	n.SetID(first.ID)
	n.SetName("Second")
	if _, err := n.Insert(ctx, ex); err == nil {
		t.Fatal("inserting a duplicate primary key must fail without an explicit conflict target")
	}
}
