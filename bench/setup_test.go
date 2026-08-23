package bench

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gsoultan/raorm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	nUsers = 50_000
	nOrgs  = 100
)

var (
	pool *pgxpool.Pool
	ids  [][16]byte
	orgs [][16]byte
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("RAORM_DSN")
	if dsn == "" {
		fmt.Println("RAORM_DSN unset; skipping")
		os.Exit(0)
	}
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dsn)
	must(err)
	// One pool, shared by every implementation under test: a benchmark
	// comparison must match capacity on both sides.
	cfg.MinConns, cfg.MaxConns = 8, 8
	// Through raorm's constructor, so the fast parameter encoders are
	// installed — otherwise the = ANY numbers below measure pgx's generic
	// array codec and say nothing about raorm.
	pool, err = pgxdrv.NewPoolConfig(ctx, cfg)
	must(err)
	defer pool.Close()

	must(seed(ctx))
	os.Exit(m.Run())
}

func seed(ctx context.Context) error {
	sql, err := os.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		return err
	}

	orgs = make([][16]byte, nOrgs)
	for i := range orgs {
		orgs[i][0], orgs[i][1] = byte(i), byte(i>>8)
		orgs[i][15] = 0xAA
	}

	ids = make([][16]byte, nUsers)
	rows := make([][]any, 0, nUsers)
	base := time.Now().Add(-365 * 24 * time.Hour)
	statuses := []string{"active", "pending", "suspended"}

	for i := 0; i < nUsers; i++ {
		var id [16]byte
		id[0], id[1], id[2], id[3] = byte(i), byte(i>>8), byte(i>>16), byte(i>>24)
		id[15] = 0xBB
		ids[i] = id

		var age any
		if i%7 != 0 { // ~14% NULL, so the nullable path is exercised
			age = int32(18 + i%60)
		}
		rows = append(rows, []any{
			id, orgs[i%nOrgs],
			fmt.Sprintf("user%06d@corp.com", i),
			fmt.Sprintf("User %06d", i),
			age,
			statuses[i%3],
			base.Add(time.Duration(i) * time.Minute),
			base.Add(time.Duration(i) * time.Minute),
		})
	}

	_, err = pool.CopyFrom(ctx,
		[]string{"users"},
		[]string{"id", "org_id", "email", "name", "age", "status", "created_at", "updated_at"},
		pgx.CopyFromRows(rows))
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, "ANALYZE users")
	return err
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
