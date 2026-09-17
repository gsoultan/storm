package storm_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/migrate"
)

// Partitioning, and the rule that matters more than the syntax: a partition
// storm did not create is a partition storm does not delete.

type event struct {
	storm.Model
	OccurredAt time.Time
	Kind       string
}

func (e *event) Schema(t *storm.Table) {
	// PostgreSQL requires the partition key in every unique key, so the model
	// must say so itself — storm will not widen one behind your back.
	t.PrimaryKey(&e.ID, &e.OccurredAt)
	t.PartitionBy(storm.RangePartition, &e.OccurredAt)
}

func TestPartitionedTableRoundTrips(t *testing.T) {
	c := connect(t)
	ctx := context.Background()
	const ns = "storm_part_rt"
	t.Cleanup(func() { _, _ = c.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE") })
	if _, err := c.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE; CREATE SCHEMA "+ns); err != nil {
		t.Fatal(err)
	}
	s, err := storm.Build(&event{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := migrate.ForWith(ctx, c, ns, s, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.SQL(), "PARTITION BY RANGE") {
		t.Fatalf("the table was not created partitioned:\n%s", plan.SQL())
	}
	if _, err := c.Exec(ctx, "SET search_path TO "+ns); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, plan.SQL()); err != nil {
		t.Fatalf("plan does not apply: %v\n---\n%s", err, plan.SQL())
	}
	again, err := migrate.ForWith(ctx, c, ns, s, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("a partitioned table does not round-trip:\n%s", again.SQL())
	}
}

// The one that protects data. Partitions are made by a scheduled job, so they
// exist in the database and in no model — which is exactly what an ordinary
// table with a deleted declaration looks like. If storm cannot tell them
// apart, the friendly migration it offers drops last month's rows.
func TestAnUndeclaredPartitionIsNotDropped(t *testing.T) {
	c := connect(t)
	ctx := context.Background()
	const ns = "storm_part_keep"
	t.Cleanup(func() { _, _ = c.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE") })
	if _, err := c.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE; CREATE SCHEMA "+ns); err != nil {
		t.Fatal(err)
	}
	s, err := storm.Build(&event{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := migrate.ForWith(ctx, c, ns, s, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "SET search_path TO "+ns); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, plan.SQL()); err != nil {
		t.Fatal(err)
	}

	// The scheduled job runs. The model does not know about this table and
	// never will.
	if _, err := c.Exec(ctx, `
		CREATE TABLE events_202609 PARTITION OF events
		FOR VALUES FROM ('2026-09-01') TO ('2026-10-01');
		INSERT INTO events (id, created_at, updated_at, occurred_at, kind)
		VALUES (gen_random_uuid(), now(), now(), '2026-09-15', 'login');`); err != nil {
		t.Fatal(err)
	}

	after, err := migrate.ForWith(ctx, c, ns, s, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(after.SQL(), "events_202609") {
		t.Fatalf("the plan touches a partition no model declared:\n%s", after.SQL())
	}
	if !after.Empty() {
		t.Fatalf("a new partition made the schema look drifted:\n%s", after.SQL())
	}

	// And the row is still there, which is the thing the test is really about.
	var n int
	if err := c.QueryRow(ctx, "SELECT count(*) FROM events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected the partitioned row to survive, got %d rows", n)
	}
}

// Partitioning cannot be altered, so a model that disagrees with the database
// must say so rather than emit an ALTER that silently does nothing.
func TestChangingPartitioningIsRefused(t *testing.T) {
	c := connect(t)
	ctx := context.Background()
	const ns = "storm_part_change"
	t.Cleanup(func() { _, _ = c.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE") })
	if _, err := c.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE; CREATE SCHEMA "+ns); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "SET search_path TO "+ns); err != nil {
		t.Fatal(err)
	}
	// A plain table where the model wants a partitioned one.
	if _, err := c.Exec(ctx, `CREATE TABLE events (
		id uuid NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now(),
		occurred_at timestamptz NOT NULL, kind text NOT NULL,
		PRIMARY KEY (id, occurred_at))`); err != nil {
		t.Fatal(err)
	}
	s, err := storm.Build(&event{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := migrate.ForWith(ctx, c, ns, s, migrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.SQL(), "cannot change the partitioning") {
		t.Fatalf("a partitioning mismatch was not reported:\n%s", plan.SQL())
	}
}
