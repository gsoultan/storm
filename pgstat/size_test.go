package pgstat_test

import (
	"context"
	"os"
	"testing"

	"github.com/gsoultan/storm/pgstat"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A table with rows in it takes space, and the database holding it takes at
// least as much.
func TestSizesCountWhatATableHolds(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const ns = "storm_pgstat_size"
	for _, stmt := range []string{
		"DROP SCHEMA IF EXISTS " + ns + " CASCADE",
		"CREATE SCHEMA " + ns,
		"CREATE TABLE " + ns + ".filled (id int PRIMARY KEY, body text)",
		"INSERT INTO " + ns + ".filled SELECT n, repeat('x', 200) FROM generate_series(1, 1000) AS n",
		"CREATE TABLE " + ns + ".empty (id int)",
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE") })
	ex := pgxdrv.Pool{P: pool}

	sizes, err := pgstat.TableSizes(ctx, ex, ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 2 || sizes["filled"] <= sizes["empty"] || sizes["filled"] < 200*1000 {
		t.Fatalf("table sizes %v: want both tables, the filled one holding at least its 200 kB", sizes)
	}
	total, err := pgstat.DatabaseSize(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if total < sizes["filled"] {
		t.Fatalf("the database is %d bytes, smaller than the %d one of its tables takes", total, sizes["filled"])
	}

	pool.Close()
	if _, err := pgstat.DatabaseSize(ctx, ex); err == nil {
		t.Fatal("a closed pool read a size")
	}
	if _, err := pgstat.TableSizes(ctx, ex, ns); err == nil {
		t.Fatal("a closed pool read the table sizes")
	}
}
