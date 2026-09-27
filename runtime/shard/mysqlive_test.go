package shard_test

// Sharding on a second dialect.
//
// Everything else in this package's live suite runs on PostgreSQL. That
// proves the router works over pgxdrv, and proves nothing about the claim
// that actually matters — that routing is dialect-independent because it
// routes EXECUTORS and never looks at SQL. A second adapter is what turns
// that from an argument into evidence.
//
// It also exercises the half of ADR-0011 that is easiest to get wrong per
// adapter: a Set takes runtime.DB, so mydrv.Pool has to satisfy StartTx with
// the shared signature, and its Tx has to satisfy the shared endings.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/mydrv"
	"github.com/gsoultan/storm/runtime/shard"
)

func mysqlShards(t *testing.T) (*shard.Set, []*mydrv.Pool) {
	t.Helper()
	addr := os.Getenv("STORM_MYSQL_ADDR")
	if addr == "" {
		t.Skip("STORM_MYSQL_ADDR unset")
	}
	ctx := context.Background()

	cfg := func(db string) mydrv.Config {
		return mydrv.Config{
			Addr: addr, User: "root", Password: "storm", Database: db,
			TLS:                                 mydrv.TLSPreferred,
			AllowCleartextPasswordOverPlaintext: true,
			MaxConns:                            2,
		}
	}

	// Fatal, not Skip: the address says a server is meant to be there, and a
	// suite that skips itself when it cannot connect reports as passed.
	admin, err := mydrv.NewPool(ctx, cfg("storm"))
	if err != nil {
		t.Fatalf("STORM_MYSQL_ADDR is set and the server refused: %v", err)
	}
	defer admin.Close()

	names := make([]string, shardCount)
	pools := make([]*mydrv.Pool, shardCount)
	dbs := make([]runtime.DB, shardCount)

	for i := range shardCount {
		name := "storm_myshard_" + string(rune('0'+i))
		names[i] = name
		// MySQL has no DROP DATABASE IF EXISTS ... FORCE; the pools this test
		// owns are the only connections to these, and they are closed first.
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name, nil); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+name, nil); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if pools[i], err = mydrv.NewPool(ctx, cfg(name)); err != nil {
			t.Fatalf("connect %s: %v", name, err)
		}
		dbs[i] = pools[i]

		// BINARY(16) rather than a CHAR uuid: it is what storm binds a
		// storm.UUID as on MySQL, so the rows this test writes have the shape
		// generated code would write.
		if _, err := pools[i].Exec(ctx,
			"CREATE TABLE orders (tenant_id BINARY(16) NOT NULL, n INT NOT NULL)", nil); err != nil {
			t.Fatalf("create table on %s: %v", name, err)
		}
	}

	t.Cleanup(func() {
		for _, p := range pools {
			p.Close()
		}
		a, err := mydrv.NewPool(context.Background(), cfg("storm"))
		if err != nil {
			return
		}
		defer a.Close()
		for _, n := range names {
			_, _ = a.Exec(context.Background(), "DROP DATABASE IF EXISTS "+n, nil)
		}
	})

	set, err := shard.New(shard.Jump(shardCount), dbs...)
	if err != nil {
		t.Fatal(err)
	}
	return set, pools
}

// countOn reads how many rows one shard's database holds for a tenant.
func countOn(t *testing.T, ctx context.Context, ex runtime.Executor, id [16]byte) int {
	t.Helper()
	rows, err := ex.Query(ctx, "SELECT n FROM orders WHERE tenant_id = ?", []any{id[:]})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// The same claim the PostgreSQL suite makes, on a different wire protocol: a
// row written through For(key) is in the database the locator names, and in
// no other.
func TestLiveMySQLRowsLandOnTheirOwnShard(t *testing.T) {
	set, pools := mysqlShards(t)
	ctx := t.Context()

	placed := map[[16]byte]shard.ID{}
	for i, id := range tenants {
		key := shard.UUIDKey(id)
		ex, err := set.For(key)
		if err != nil {
			t.Fatal(err)
		}
		byLocator, err := shard.Jump(shardCount).Locate(key)
		if err != nil {
			t.Fatal(err)
		}
		if ex.Shard() != byLocator {
			t.Fatalf("tenant %x: For routed to %d, the locator says %d",
				id[0], ex.Shard(), byLocator)
		}
		if _, err := ex.Exec(ctx,
			"INSERT INTO orders (tenant_id, n) VALUES (?, ?)", []any{id[:], int32(i)}); err != nil {
			t.Fatalf("insert for %x: %v", id[0], err)
		}
		placed[id] = ex.Shard()
	}

	used := map[shard.ID]bool{}
	for _, s := range placed {
		used[s] = true
	}
	if len(used) < shardCount {
		t.Fatalf("only %d of %d shards were used; this test cannot see a routing bug",
			len(used), shardCount)
	}

	for id, want := range placed {
		for i, p := range pools {
			got := countOn(t, ctx, p, id)
			switch {
			case shard.ID(i) == want && got != 1:
				t.Errorf("tenant %x routed to shard %d, found %d rows there", id[0], i, got)
			case shard.ID(i) != want && got != 0:
				t.Errorf("tenant %x is on shard %d as well as %d", id[0], i, want)
			}
		}
	}
}

// mydrv.Pool satisfies runtime.DB, and its Tx satisfies the shared endings —
// asserted at compile time in the package, and exercised here, because
// "START TRANSACTION goes through the text protocol" is the kind of detail
// that makes an adapter's transaction behave differently from the contract.
func TestLiveMySQLInTxCommitsAndRollsBackOnOneShard(t *testing.T) {
	set, _ := mysqlShards(t)
	ctx := t.Context()

	id := tenants[0]
	key := shard.UUIDKey(id)

	if err := set.InTx(ctx, key, func(ex shard.Bound) error {
		_, err := ex.Exec(ctx,
			"INSERT INTO orders (tenant_id, n) VALUES (?, 1)", []any{id[:]})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	routed, err := set.For(key)
	if err != nil {
		t.Fatal(err)
	}
	if n := countOn(t, ctx, routed, id); n != 1 {
		t.Fatalf("%d rows after a committed InTx, want 1", n)
	}

	boom := errors.New("boom")
	err = set.InTx(ctx, key, func(ex shard.Bound) error {
		if _, err := ex.Exec(ctx,
			"INSERT INTO orders (tenant_id, n) VALUES (?, 2)", []any{id[:]}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if n := countOn(t, ctx, routed, id); n != 1 {
		t.Errorf("%d rows after a rolled-back InTx, want 1 — MySQL did not roll back", n)
	}
}

// The wrong shard answers with silence here too. Stated per dialect because
// it is the premise the whole compile-time Bound requirement rests on, and a
// dialect where the wrong shard ERRORED would not need it.
func TestLiveMySQLWrongShardAnswersWithSilence(t *testing.T) {
	set, _ := mysqlShards(t)
	ctx := t.Context()

	id := tenants[0]
	right, err := set.For(shard.UUIDKey(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := right.Exec(ctx,
		"INSERT INTO orders (tenant_id, n) VALUES (?, 1)", []any{id[:]}); err != nil {
		t.Fatal(err)
	}

	wrong, err := set.ForID((right.Shard() + 1) % shardCount)
	if err != nil {
		t.Fatal(err)
	}
	if n := countOn(t, ctx, wrong, id); n != 0 {
		t.Fatalf("the wrong shard returned %d rows", n)
	}
}
