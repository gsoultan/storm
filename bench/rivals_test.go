package bench

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/gsoultan/storm/bench/entbench"
	entuser "github.com/gsoultan/storm/bench/entbench/user"
	sqlcgen "github.com/gsoultan/storm/bench/sqlcbench/gen"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Rivals run the same workload against the same database. Bun and GORM sit on
// database/sql, which is how they are actually deployed, so their pools are
// pinned to the same 8 connections as pgxpool: a benchmark comparison must
// match capacity on both sides.

// ---- sqlc: shares the pgxpool directly ----

func sqlcQ() *sqlcgen.Queries { return sqlcgen.New(pool) }

// ---- Bun & GORM: database/sql over the pgx stdlib driver ----

type BunUser struct {
	bun.BaseModel `bun:"table:users,alias:u"`

	ID        uuid.UUID `bun:"id,pk"`
	OrgID     uuid.UUID `bun:"org_id"`
	Email     string    `bun:"email"`
	Name      string    `bun:"name"`
	Age       *int32    `bun:"age"`
	Status    string    `bun:"status"`
	CreatedAt time.Time `bun:"created_at"`
	UpdatedAt time.Time `bun:"updated_at"`
}

type GormUser struct {
	ID        uuid.UUID `gorm:"column:id;primaryKey"`
	OrgID     uuid.UUID `gorm:"column:org_id"`
	Email     string    `gorm:"column:email"`
	Name      string    `gorm:"column:name"`
	Age       *int32    `gorm:"column:age"`
	Status    string    `gorm:"column:status"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (GormUser) TableName() string { return "users" }

var (
	stdDB  *sql.DB
	bunDB  *bun.DB
	gormDB *gorm.DB
	entDB  *entbench.Client
)

func initRivals(tb testing.TB) {
	if bunDB != nil {
		return
	}
	cfg := pool.Config().ConnConfig.Copy()
	stdDB = stdlib.OpenDB(*cfg)
	stdDB.SetMaxOpenConns(8)
	stdDB.SetMaxIdleConns(8)

	bunDB = bun.NewDB(stdDB, pgdialect.New())

	var err error
	entDB = entbench.NewClient(entbench.Driver(entsql.OpenDB(dialect.Postgres, stdDB)))

	gormDB, err = gorm.Open(postgres.New(postgres.Config{Conn: stdDB}), &gorm.Config{
		Logger:                 gormlogger.Discard,
		SkipDefaultTransaction: true,
		PrepareStmt:            true, // give GORM its best case
	})
	if err != nil {
		tb.Fatal(err)
	}
}

// ---- Get: one row by primary key ----

func BenchmarkGet_Sqlc(b *testing.B) {
	ctx := context.Background()
	q := sqlcQ()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var id pgtype.UUID
		id.Bytes, id.Valid = ids[i%len(ids)], true
		if _, err := q.GetUser(ctx, id); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGet_Bun(b *testing.B) {
	initRivals(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var u BunUser
		if err := bunDB.NewSelect().Model(&u).
			Where("id = ?", uuid.UUID(ids[i%len(ids)])).Scan(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGet_Gorm(b *testing.B) {
	initRivals(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var u GormUser
		if err := gormDB.First(&u, "id = ?", uuid.UUID(ids[i%len(ids)])).Error; err != nil {
			b.Fatal(err)
		}
	}
}

// ---- Scan 1000 rows ----

func BenchmarkScan1000_Sqlc(b *testing.B) {
	ctx := context.Background()
	q := sqlcQ()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := q.ListByStatus(ctx, sqlcgen.ListByStatusParams{Status: "active", Limit: 1000})
		if err != nil || len(out) != 1000 {
			b.Fatal(err, len(out))
		}
	}
}

func BenchmarkScan1000_Bun(b *testing.B) {
	initRivals(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := make([]BunUser, 0, 1000)
		if err := bunDB.NewSelect().Model(&out).
			Where("status = ?", "active").
			OrderExpr("created_at DESC, id").
			Limit(1000).Scan(ctx); err != nil {
			b.Fatal(err)
		}
		if len(out) != 1000 {
			b.Fatal(len(out))
		}
	}
}

func BenchmarkScan1000_Gorm(b *testing.B) {
	initRivals(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := make([]GormUser, 0, 1000)
		if err := gormDB.Where("status = ?", "active").
			Order("created_at DESC, id").Limit(1000).Find(&out).Error; err != nil {
			b.Fatal(err)
		}
		if len(out) != 1000 {
			b.Fatal(len(out))
		}
	}
}

// Ent, genuinely generated — its client is 164K of code for this one table,
// which is R2 ("generated-code volume becomes Ent's disease") measured rather
// than asserted. Same pool, same workload as every other rival.

func BenchmarkGet_Ent(b *testing.B) {
	initRivals(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if _, err := entDB.User.Get(ctx, ids[i%len(ids)]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScan1000_Ent(b *testing.B) {
	initRivals(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		out, err := entDB.User.Query().
			Where(entuser.StatusEQ("active")).
			Order(entuser.ByCreatedAt(entsql.OrderDesc()), entuser.ByID()).
			Limit(1000).
			All(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if len(out) != 1000 {
			b.Fatal(len(out))
		}
	}
}
