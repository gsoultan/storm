package shard_test

// Routing on SQL Server, against real databases.
//
// Sharding had been generated for every dialect and EXECUTED only on
// PostgreSQL and MySQL. "Routing is dialect-independent" is a claim about
// executors, and each adapter is its own evidence: this one also proves
// msdrv's StartTx is what Set.InTx rides on.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/msdrv"
	"github.com/gsoultan/storm/runtime/shard"
)

func mssqlShards(t *testing.T) (*shard.Set, []*msdrv.Pool) {
	t.Helper()
	addr := os.Getenv("STORM_MSSQL_ADDR")
	if addr == "" {
		t.Skip("STORM_MSSQL_ADDR unset")
	}
	ctx := context.Background()
	cfg := func(db string) msdrv.Config {
		return msdrv.Config{
			Addr: addr, User: "sa", Password: os.Getenv("STORM_MSSQL_PASSWORD"),
			Database: db, AppName: "storm-shard-test", MaxConns: 2,
			// The certificate SQL Server generates for itself is one Go refuses
			// to parse (msdrv.ErrNegativeSerial), as msdrv's own live tests note;
			// TLS is covered by msdrv's TLS test, not here.
			TLS: msdrv.TLSDisabled,
		}
	}
	// Fatal, not Skip: the address says a server is meant to be there, and a
	// suite that skips itself when it cannot connect reports as passed.
	admin, err := msdrv.NewPool(ctx, cfg("master"))
	if err != nil {
		t.Fatalf("STORM_MSSQL_ADDR is set and the server refused: %v", err)
	}
	defer admin.Close()

	drop := func(ex runtime.Executor, name string) error {
		_, err := ex.Exec(context.Background(), "IF DB_ID(N'"+name+"') IS NOT NULL BEGIN "+
			"ALTER DATABASE ["+name+"] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; "+
			"DROP DATABASE ["+name+"]; END", nil)
		return err
	}
	names := make([]string, shardCount)
	pools := make([]*msdrv.Pool, shardCount)
	dbs := make([]runtime.DB, shardCount)
	for i := range shardCount {
		name := "storm_msshard_" + string(rune('0'+i))
		names[i] = name
		if err := drop(admin, name); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
		if _, err := admin.Exec(ctx, "CREATE DATABASE ["+name+"]", nil); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if pools[i], err = msdrv.NewPool(ctx, cfg(name)); err != nil {
			t.Fatalf("connect %s: %v", name, err)
		}
		dbs[i] = pools[i]
		if _, err := pools[i].Exec(ctx,
			"CREATE TABLE orders (tenant_id BINARY(16) NOT NULL, n INT NOT NULL)", nil); err != nil {
			t.Fatalf("create table on %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		for _, p := range pools {
			p.Close()
		}
		a, err := msdrv.NewPool(context.Background(), cfg("master"))
		if err != nil {
			return
		}
		defer a.Close()
		for _, n := range names {
			_ = drop(a, n)
		}
	})

	set, err := shard.New(shard.Jump(shardCount), dbs...)
	if err != nil {
		t.Fatal(err)
	}
	return set, pools
}

func msCountOn(t *testing.T, ctx context.Context, ex runtime.Executor, id [16]byte) int {
	t.Helper()
	rows, err := ex.Query(ctx, "SELECT n FROM orders WHERE tenant_id = @p1", []any{id[:]})
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

func TestLiveMSSQLRowsLandOnTheirOwnShard(t *testing.T) {
	set, pools := mssqlShards(t)
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
			t.Fatalf("tenant %x: For routed to %d, the locator says %d", id[0], ex.Shard(), byLocator)
		}
		if _, err := ex.Exec(ctx,
			"INSERT INTO orders (tenant_id, n) VALUES (@p1, @p2)", []any{id[:], int32(i)}); err != nil {
			t.Fatalf("insert for %x: %v", id[0], err)
		}
		placed[id] = ex.Shard()
	}
	used := map[shard.ID]bool{}
	for _, s := range placed {
		used[s] = true
	}
	if len(used) < shardCount {
		t.Fatalf("only %d of %d shards were used; this test cannot see a routing bug", len(used), shardCount)
	}
	for id, want := range placed {
		for i, p := range pools {
			got := msCountOn(t, ctx, p, id)
			switch {
			case shard.ID(i) == want && got != 1:
				t.Errorf("tenant %x routed to shard %d, found %d rows there", id[0], i, got)
			case shard.ID(i) != want && got != 0:
				t.Errorf("tenant %x is on shard %d as well as %d", id[0], i, want)
			}
		}
	}
}

func TestLiveMSSQLInTxCommitsAndRollsBackOnOneShard(t *testing.T) {
	set, _ := mssqlShards(t)
	ctx := t.Context()
	id := tenants[0]
	key := shard.UUIDKey(id)
	if err := set.InTx(ctx, key, func(ex shard.Bound) error {
		_, err := ex.Exec(ctx, "INSERT INTO orders (tenant_id, n) VALUES (@p1, 1)", []any{id[:]})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	routed, err := set.For(key)
	if err != nil {
		t.Fatal(err)
	}
	if n := msCountOn(t, ctx, routed, id); n != 1 {
		t.Fatalf("%d rows after a committed InTx, want 1", n)
	}
	boom := errors.New("boom")
	err = set.InTx(ctx, key, func(ex shard.Bound) error {
		if _, err := ex.Exec(ctx, "INSERT INTO orders (tenant_id, n) VALUES (@p1, 2)", []any{id[:]}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if n := msCountOn(t, ctx, routed, id); n != 1 {
		t.Errorf("%d rows after a rolled-back InTx, want 1 — SQL Server did not roll back", n)
	}
}

func TestLiveMSSQLWrongShardAnswersWithSilence(t *testing.T) {
	set, _ := mssqlShards(t)
	ctx := t.Context()
	id := tenants[0]
	right, err := set.For(shard.UUIDKey(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := right.Exec(ctx, "INSERT INTO orders (tenant_id, n) VALUES (@p1, 1)", []any{id[:]}); err != nil {
		t.Fatal(err)
	}
	wrong, err := set.ForID((right.Shard() + 1) % shardCount)
	if err != nil {
		t.Fatal(err)
	}
	if n := msCountOn(t, ctx, wrong, id); n != 0 {
		t.Fatalf("the wrong shard returned %d rows", n)
	}
}
