package shard_test

// Sharding, against two real databases.
//
// Every other test in this package uses a stub, and a stub can only catch a
// wrong CALL. The failure sharding exists to prevent is a wrong ANSWER: a row
// written to the database that will never be asked for it, and a read that
// returns rows — just not all of them, with no error, no SQLSTATE and no log
// line. Proving that cannot happen needs two databases with different rows in
// them.
//
// Two DATABASES on one server rather than two servers. What is under test is
// storm's routing and its refusals, not PostgreSQL's process isolation: the
// pools are separate, the connections are separate, and the rows are in
// different places, which is everything the code being tested can observe.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/gsoultan/storm/runtime/shard"
	"github.com/jackc/pgx/v5/pgxpool"
)

// liveOrder is the model the migration test brings up on every shard.
type liveOrder struct {
	storm.Model

	TenantID storm.UUID
	Total    int32
}

func (o *liveOrder) Schema(t *storm.Table) { t.ShardKey(&o.TenantID) }

const shardCount = 2

// tenants are fixed rather than random so a failure names the same key twice
// and can be reproduced from the message.
var tenants = [][16]byte{
	{0x01}, {0x02}, {0x03}, {0x04}, {0x05}, {0x06}, {0x07}, {0x08},
	{0x09}, {0x0a}, {0x0b}, {0x0c}, {0x0d}, {0x0e}, {0x0f}, {0x10},
}

// liveShards builds a Set over shardCount freshly created databases.
func liveShards(t *testing.T) (*shard.Set, []pgxdrv.Pool) {
	t.Helper()
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	names := make([]string, shardCount)
	pools := make([]pgxdrv.Pool, shardCount)
	dbs := make([]runtime.DB, shardCount)

	for i := range shardCount {
		name := "storm_shard_" + string(rune('0'+i))
		names[i] = name
		// DROP first: a previous run that failed mid-way leaves a database
		// behind, and reusing it would let stale rows answer this run's
		// assertions.
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}

		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Database = name
		// Bounded on purpose. pgxpool defaults to max(4, NumCPU) per pool,
		// and this suite opens one pool PER SHARD per test, against a server
		// that `go test ./...` is also running the tool and migrate suites
		// against in parallel. Nothing here needs concurrency; what it needs
		// is not to be the reason someone else's test cannot connect.
		cfg.MaxConns = 2
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connect %s: %v", name, err)
		}
		pools[i] = pgxdrv.Pool{P: p}
		dbs[i] = pools[i]

		if _, err := pools[i].Exec(ctx,
			"CREATE TABLE orders (tenant_id uuid NOT NULL, n int NOT NULL)", nil); err != nil {
			t.Fatalf("create table on %s: %v", name, err)
		}
	}

	t.Cleanup(func() {
		for _, p := range pools {
			p.P.Close()
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

	set, err := shard.New(shard.Jump(shardCount), dbs...)
	if err != nil {
		t.Fatal(err)
	}
	return set, pools
}

// tenantsOn reads the tenant ids actually present in one shard's database.
func tenantsOn(t *testing.T, ctx context.Context, p pgxdrv.Pool) map[[16]byte]bool {
	t.Helper()
	rows, err := p.P.Query(ctx, "SELECT tenant_id FROM orders")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	out := map[[16]byte]bool{}
	for rows.Next() {
		var id [16]byte
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The claim: a row written through For(key) is in the database the locator
// names, and in no other. Both halves matter — "it is here" without "it is
// not there" would pass if every write went to every shard.
func TestLiveRowsLandOnTheirOwnShardAndNowhereElse(t *testing.T) {
	set, pools := liveShards(t)
	ctx := t.Context()

	want := make([]map[[16]byte]bool, shardCount)
	for i := range want {
		want[i] = map[[16]byte]bool{}
	}

	for i, id := range tenants {
		key := shard.UUIDKey(id)
		ex, err := set.For(key)
		if err != nil {
			t.Fatalf("route %x: %v", id[0], err)
		}
		// The Bound must agree with the LOCATOR, computed independently.
		// Without this the test only proves For is self-consistent: a For
		// that ignored the locator and used its own stable mapping would
		// still put each row where its own Bound said, and pass.
		byLocator, err := shard.Jump(shardCount).Locate(key)
		if err != nil {
			t.Fatal(err)
		}
		if ex.Shard() != byLocator {
			t.Fatalf("tenant %x: For routed to shard %d, the locator says %d",
				id[0], ex.Shard(), byLocator)
		}
		if _, err := ex.Exec(ctx,
			"INSERT INTO orders (tenant_id, n) VALUES ($1, $2)", []any{id, i}); err != nil {
			t.Fatalf("insert for %x: %v", id[0], err)
		}
		want[ex.Shard()][id] = true
	}

	// Both shards must have been used, or the test proves nothing about
	// routing — it would pass on a Set that sent everything to shard 0.
	for i, w := range want {
		if len(w) == 0 {
			t.Fatalf("shard %d took none of %d tenants; this test cannot see a routing bug",
				i, len(tenants))
		}
	}

	for i, p := range pools {
		got := tenantsOn(t, ctx, p)
		for id := range want[i] {
			if !got[id] {
				t.Errorf("tenant %x was routed to shard %d and is not there", id[0], i)
			}
		}
		for id := range got {
			if !want[i][id] {
				t.Errorf("tenant %x is on shard %d and was never routed there", id[0], i)
			}
		}
	}
}

// The wrong-answer shape, stated directly: reading a tenant from the shard it
// does NOT live on returns no rows rather than an error. This is what the
// compile-time Bound requirement exists to make unwritable, and asserting it
// here is what makes that requirement worth having.
func TestLiveTheWrongShardAnswersWithSilenceNotAnError(t *testing.T) {
	set, _ := liveShards(t)
	ctx := t.Context()

	id := tenants[0]
	right, err := set.For(shard.UUIDKey(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := right.Exec(ctx,
		"INSERT INTO orders (tenant_id, n) VALUES ($1, 1)", []any{id}); err != nil {
		t.Fatal(err)
	}

	wrong, err := set.ForID((right.Shard() + 1) % shardCount)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := wrong.Query(ctx, "SELECT n FROM orders WHERE tenant_id = $1", []any{id})
	if err != nil {
		t.Fatalf("the wrong shard ERRORED, which would make this bug findable: %v", err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the wrong shard returned %d rows", n)
	}
	// No error, no rows: a silent wrong answer. Exactly what shard.Bound is
	// for, and why it is a compile error rather than a runtime check.
}

func TestLiveEachVisitsEveryShardsOwnRows(t *testing.T) {
	set, _ := liveShards(t)
	ctx := t.Context()

	for i, id := range tenants {
		ex, err := set.For(shard.UUIDKey(id))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ex.Exec(ctx,
			"INSERT INTO orders (tenant_id, n) VALUES ($1, $2)", []any{id, i}); err != nil {
			t.Fatal(err)
		}
	}

	total := 0
	visited := 0
	err := set.Each(ctx, func(ctx context.Context, _ shard.ID, ex shard.Bound) error {
		visited++
		rows, err := ex.Query(ctx, "SELECT n FROM orders", nil)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			total++
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if visited != shardCount {
		t.Errorf("Each visited %d shards, want %d", visited, shardCount)
	}
	// Summing a per-shard count is the combination that IS sound, which is
	// the one Each is documented for.
	if total != len(tenants) {
		t.Errorf("counted %d rows across shards, want %d", total, len(tenants))
	}
}

func TestLiveInTxCommitsAndRollsBackOnOneShard(t *testing.T) {
	set, pools := liveShards(t)
	ctx := t.Context()

	id := tenants[0]
	key := shard.UUIDKey(id)

	if err := set.InTx(ctx, key, func(ex shard.Bound) error {
		_, err := ex.Exec(ctx, "INSERT INTO orders (tenant_id, n) VALUES ($1, 1)", []any{id})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	routed, _ := set.For(key)
	if got := len(tenantsOn(t, ctx, pools[routed.Shard()])); got != 1 {
		t.Fatalf("%d rows after a committed InTx, want 1", got)
	}

	boom := errors.New("boom")
	err := set.InTx(ctx, key, func(ex shard.Bound) error {
		if _, err := ex.Exec(ctx,
			"INSERT INTO orders (tenant_id, n) VALUES ($1, 2)", []any{id}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}

	rows, err := routed.Query(ctx, "SELECT n FROM orders", nil)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if n != 1 {
		t.Errorf("%d rows after a rolled-back InTx, want 1", n)
	}
}

// The cross-shard refusal, with Bounds from a real Set rather than Pin — so
// what is proven is that two genuinely different databases are refused, not
// that two different integers are.
func TestLiveCrossShardUnitIsRefused(t *testing.T) {
	set, _ := liveShards(t)

	// Two tenants the locator puts in different places. Finding them here
	// rather than assuming tenants[0] and tenants[1] differ keeps the test
	// honest if the hash ever changes.
	var a, b shard.Bound
	for _, id := range tenants {
		ex, err := set.For(shard.UUIDKey(id))
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

	u := shard.NewUnit(map[string]int{"orders": 0})
	if err := u.Add(a, "orders", runtime.BatchOp{SQL: "INSERT INTO orders VALUES ($1, 1)"}); err != nil {
		t.Fatalf("the first Add was refused: %v", err)
	}
	err := u.Add(b, "orders", runtime.BatchOp{SQL: "INSERT INTO orders VALUES ($1, 2)"})
	if !errors.Is(err, shard.ErrCrossShard) {
		t.Fatalf("err = %v, want ErrCrossShard", err)
	}
	if !strings.Contains(err.Error(), "a unit of work is one shard") {
		t.Errorf("the refusal does not state the rule: %v", err)
	}
}

// The unit of work that SUCCEEDS. Every refusal a Unit makes is tested above;
// this is the path the refusals exist to protect: several writes for one
// tenant, flushed as one batch, landing on that tenant's shard and nowhere else.
func TestLiveUnitFlushLandsEveryWriteOnItsShard(t *testing.T) {
	set, pools := liveShards(t)
	ctx := context.Background()
	tenant := tenants[0]
	ex, err := set.For(shard.UUIDKey(tenant))
	if err != nil {
		t.Fatal(err)
	}

	u := shard.NewUnit(map[string]int{"orders": 0})
	for n := 1; n <= 3; n++ {
		op := runtime.BatchOp{SQL: "INSERT INTO orders VALUES ($1, $2)", Args: []any{tenant, n}}
		if err := u.Add(ex, "orders", op); err != nil {
			t.Fatalf("Add %d: %v", n, err)
		}
	}
	affected, err := u.Flush(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(affected) != 3 {
		t.Fatalf("flush reported %d statements, want 3", len(affected))
	}
	for i, a := range affected {
		if a != 1 {
			t.Errorf("statement %d affected %d rows, want 1", i, a)
		}
	}

	for i, p := range pools {
		var n int
		if err := p.P.QueryRow(ctx, "SELECT count(*) FROM orders WHERE tenant_id = $1", tenant).Scan(&n); err != nil {
			t.Fatal(err)
		}
		want := 0
		if shard.ID(i) == ex.Shard() {
			want = 3
		}
		if n != want {
			t.Errorf("shard %d holds %d of the tenant's rows, want %d", i, n, want)
		}
	}
}

// Migrating a shard set: storm does not do it for you, and this is what doing
// it looks like.
//
// A Set holds runtime.DB values and migrate.AutoPool takes a *pgxpool.Pool,
// so there is no Set.Migrate and there should not be: migration happens
// BEFORE a Set exists, while the caller still holds the pools they are about
// to hand over. The loop below is the whole story, and the reason it is a
// test rather than a paragraph is that "the two features compose" is a claim.
//
// What storm does NOT coordinate, and what an operator has to: migrating four
// shards is four migrations. Auto takes an advisory lock per DATABASE, so
// several app instances racing to migrate ONE shard is safe; nothing makes
// the four atomic with each other. A rollout that dies after shard 1 leaves
// shards 2 and 3 on the old schema, serving reads that succeed.
func TestLiveMigrateEveryShardThenBuildTheSet(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := t.Context()

	model, err := storm.Build(&liveOrder{})
	if err != nil {
		t.Fatal(err)
	}

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	var (
		names = make([]string, shardCount)
		raw   = make([]*pgxpool.Pool, shardCount)
		dbs   = make([]runtime.DB, shardCount)
	)
	for i := range shardCount {
		name := "storm_migshard_" + string(rune('0'+i))
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

	// THE LOOP. Every shard gets the same model, one at a time, before any of
	// them serves a query.
	for i, p := range raw {
		res, err := migrate.AutoPool(ctx, p, model, migrate.AutoOptions{})
		if err != nil {
			t.Fatalf("migrate shard %d: %v", i, err)
		}
		if len(res.Applied) == 0 {
			t.Errorf("shard %d: migrating an empty database applied nothing", i)
		}
	}

	// Idempotent on a second pass, which is what makes the loop safe to run
	// at every startup rather than once by hand.
	for i, p := range raw {
		res, err := migrate.AutoPool(ctx, p, model, migrate.AutoOptions{})
		if err != nil {
			t.Fatalf("re-migrate shard %d: %v", i, err)
		}
		if len(res.Applied) != 0 {
			t.Errorf("shard %d: a second migration applied %d steps, want 0",
				i, len(res.Applied))
		}
	}

	// And now the Set, over schemas that are actually there.
	set, err := shard.New(shard.Jump(shardCount), dbs...)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range tenants {
		ex, err := set.For(shard.UUIDKey(id))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ex.Exec(ctx,
			`INSERT INTO live_orders (id, created_at, updated_at, tenant_id, total)
			 VALUES ($1, now(), now(), $2, 1)`, []any{id, id}); err != nil {
			t.Fatalf("the migrated shard would not take a write: %v", err)
		}
	}

	total := 0
	if err := set.Each(ctx, func(ctx context.Context, _ shard.ID, ex shard.Bound) error {
		rows, err := ex.Query(ctx, "SELECT tenant_id FROM live_orders", nil)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			total++
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if total != len(tenants) {
		t.Errorf("%d rows across the migrated shards, want %d", total, len(tenants))
	}
}
