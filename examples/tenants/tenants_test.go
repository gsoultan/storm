package tenants_test

// The sharded example, end to end, against two real databases.
//
// Everything else that tests sharding does it through raw SQL on a Bound.
// This is the only place the GENERATED API is exercised on a shard set — the
// query builder, the writes, ShardKeyOf, and the checking unit of work — which
// is the surface an adopter actually touches and the one where a signature
// that compiles can still be wired to the wrong executor.
//
// Skipped unless STORM_DSN names a server. It creates and drops two databases.

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/examples/tenants/model"
	"github.com/gsoultan/storm/examples/tenants/store"
	"github.com/gsoultan/storm/examples/tenants/store/currency"
	"github.com/gsoultan/storm/examples/tenants/store/order"
	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/gsoultan/storm/runtime/shard"
	"github.com/jackc/pgx/v5/pgxpool"
)

const shards = 2

var tenants = [][16]byte{{0xa1}, {0xb2}, {0xc3}, {0xd4}, {0xe5}, {0xf6}}

// setup is the whole adopter-facing arrangement: migrate every shard, then
// build the Set over the pools you just migrated.
func setup(t *testing.T) *shard.Set {
	t.Helper()
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()

	sch, err := storm.Build(model.All()...)
	if err != nil {
		t.Fatal(err)
	}

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	names := make([]string, shards)
	raw := make([]*pgxpool.Pool, shards)
	dbs := make([]runtime.DB, shards)

	for i := range shards {
		name := "storm_tenants_" + string(rune('0'+i))
		names[i] = name
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
			t.Fatal(err)
		}
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Database = name
		cfg.MaxConns = 2
		if raw[i], err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		dbs[i] = pgxdrv.Pool{P: raw[i]}
	}

	t.Cleanup(func() {
		for _, p := range raw {
			p.Close()
		}
		a, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			return
		}
		defer a.Close()
		for _, n := range names {
			_, _ = a.Exec(context.Background(), "DROP DATABASE IF EXISTS "+n+" WITH (FORCE)")
		}
	})

	// Every shard, before any of them serves a query.
	for i, p := range raw {
		if _, err := migrate.AutoPool(ctx, p, sch, migrate.AutoOptions{}); err != nil {
			t.Fatalf("migrate shard %d: %v", i, err)
		}
	}

	set, err := shard.New(shard.Jump(shards), dbs...)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// The quickstart, sharded: route, write, read back — all through generated
// calls, none of which would compile if handed a pool.
func TestGeneratedSurfaceRoutesWritesAndReads(t *testing.T) {
	set := setup(t)
	ctx := t.Context()

	for i, tid := range tenants {
		ex, err := set.For(shard.UUIDKey(tid))
		if err != nil {
			t.Fatal(err)
		}
		row := order.Row{
			ID: newID(), TenantID: tid,
			Status: "open", Total: int32(i + 1),
		}
		if err := order.Insert(ctx, ex, &row); err != nil {
			t.Fatalf("insert for %x: %v", tid[0], err)
		}
	}

	// Each tenant reads back exactly its own row, from its own shard.
	for i, tid := range tenants {
		ex, err := set.For(shard.UUIDKey(tid))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := order.New().TenantIDEq(tid).StatusEq("open").All(ctx, ex, nil)
		if err != nil {
			t.Fatalf("read for %x: %v", tid[0], err)
		}
		if len(rows) != 1 {
			t.Fatalf("tenant %x: %d rows, want 1", tid[0], len(rows))
		}
		if rows[0].Total != int32(i+1) {
			t.Errorf("tenant %x: total %d, want %d", tid[0], rows[0].Total, i+1)
		}
	}
}

// ShardKeyOf must agree with the key the caller would have built by hand —
// it exists so the shard key's name is written once, in the model, and a
// disagreement here means every row routed through it goes somewhere else.
func TestShardKeyOfAgreesWithTheHandBuiltKey(t *testing.T) {
	set := setup(t)

	for _, tid := range tenants {
		row := order.Row{ID: newID(), TenantID: tid}
		byHelper, err := set.For(order.ShardKeyOf(row))
		if err != nil {
			t.Fatal(err)
		}
		byHand, err := set.For(shard.UUIDKey(tid))
		if err != nil {
			t.Fatal(err)
		}
		if byHelper.Shard() != byHand.Shard() {
			t.Errorf("tenant %x: ShardKeyOf routes to %d, the hand-built key to %d",
				tid[0], byHelper.Shard(), byHand.Shard())
		}
	}
}

// A sharded model may read a REFERENCE table, and the read runs on the
// parent's own shard — so the reference rows have to be on every shard. That
// is the adopter's half of the arrangement, and this is what it looks like
// when it is held up.
func TestReferenceTableIsReadFromTheParentsShard(t *testing.T) {
	set := setup(t)
	ctx := t.Context()

	// Seed the reference table on EVERY shard, which is what makes it one.
	if err := set.Each(ctx, func(ctx context.Context, _ shard.ID, ex shard.Bound) error {
		return currency.Insert(ctx, ex, &currency.Row{ID: newID(), Code: "EUR"})
	}); err != nil {
		t.Fatal(err)
	}

	for _, tid := range tenants {
		ex, err := set.For(shard.UUIDKey(tid))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := currency.New().CodeEq("EUR").All(ctx, ex, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("tenant %x's shard has %d EUR rows, want 1 — the reference table "+
				"is not on every shard", tid[0], len(rows))
		}
	}
}

// The generated unit of work is the CHECKING one, because the context holds a
// sharded table. Two tenants on different shards in one unit is refused while
// the code that chose them is still on the stack.
func TestGeneratedUnitRefusesASecondShard(t *testing.T) {
	set := setup(t)

	var a, b shard.Bound
	for _, tid := range tenants {
		ex, err := set.For(shard.UUIDKey(tid))
		if err != nil {
			t.Fatal(err)
		}
		if a == nil {
			a = ex
			continue
		}
		if ex.Shard() != a.Shard() {
			b = ex
			break
		}
	}
	if b == nil {
		t.Fatal("every tenant routed to one shard; this test cannot see the refusal")
	}

	u := store.NewUnit()
	if err := u.Add(a, order.Table, order.InsertOp(order.Row{ID: newID(), Status: "open"})); err != nil {
		t.Fatalf("the first Add was refused: %v", err)
	}
	err := u.Add(b, order.Table, order.InsertOp(order.Row{ID: newID(), Status: "open"}))
	if !errors.Is(err, shard.ErrCrossShard) {
		t.Fatalf("err = %v, want ErrCrossShard", err)
	}
}

// One shard, one transaction, through the generated writes.
func TestInTxCommitsAndRollsBackThroughGeneratedWrites(t *testing.T) {
	set := setup(t)
	ctx := t.Context()

	tid := tenants[0]
	key := shard.UUIDKey(tid)

	if err := set.InTx(ctx, key, func(ex shard.Bound) error {
		return order.Insert(ctx, ex, &order.Row{
			ID: newID(), TenantID: tid, Status: "open", Total: 1,
		})
	}); err != nil {
		t.Fatal(err)
	}

	ex, err := set.For(key)
	if err != nil {
		t.Fatal(err)
	}
	n, err := order.New().TenantIDEq(tid).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d rows after a committed InTx, want 1", n)
	}

	boom := errors.New("boom")
	err = set.InTx(ctx, key, func(ex shard.Bound) error {
		if err := order.Insert(ctx, ex, &order.Row{
			ID: newID(), TenantID: tid, Status: "open", Total: 2,
		}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if n, err = order.New().TenantIDEq(tid).Count(ctx, ex); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d rows after a rolled-back InTx, want 1", n)
	}
}

// newID is a random uuid. Random rather than the quickstart's
// timestamp-derived one, because this test inserts several rows in a tight
// loop and two ids from the same nanosecond would collide on the primary key
// — a flaky failure that looks like a routing bug.
func newID() [16]byte {
	var v [16]byte
	if _, err := rand.Read(v[:]); err != nil {
		panic(err)
	}
	v[6] = (v[6] & 0x0f) | 0x40 // version 4
	v[8] = (v[8] & 0x3f) | 0x80 // variant
	return v
}
