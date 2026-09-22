package orabench

// The source of the live test that gets written INTO the generated package.
//
// A string rather than a file, for the reason codegen/mssqllive_src_test.go is
// one: it has to live beside the generator that produces the package it tests,
// and it cannot compile here because it imports a package that does not exist
// until the generator runs.
const generatedLiveSrc = `package PKG_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/oraddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/sqldrv"
	sd "IMPORTPATH"

	_ "github.com/sijms/go-ora/v2"
)

var ex runtime.Executor

func id(n byte) [16]byte {
	var u [16]byte
	u[15] = n
	return u
}

// The model, restated so the generated package's own test builds the same
// schema the generator was given.
//
// NO NULLABLE TEXT, which is the Oracle rule: an empty string is stored as
// NULL there, so "" and nil would be one value. compile/oraddl refuses the
// column and this model is what passing that refusal looks like.
type oraUser struct {
	storm.Model
	Email     string
	Name      string
	Rank      int64
	Balance   storm.Decimal
	Active    bool
	DeletedAt *time.Time
}

func (u *oraUser) Schema(t *storm.Table) {
	t.Col(&u.Email).Size(255)
	t.Col(&u.Name).Size(120)
	t.Col(&u.Balance).Numeric(18, 4)
	t.SoftDelete(&u.DeletedAt)
	t.Index(&u.Email).Unique().Where(` + "`" + `"deleted_at" IS NULL` + "`" + `)
}

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_ORACLE_DSN")
	if dsn == "" {
		os.Stderr.WriteString("STORM_ORACLE_DSN did not reach the generated package\n")
		os.Exit(1)
	}
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	defer db.Close()

	s, err := storm.Build(&oraUser{})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	stmts, err := oraddl.Statements(s)
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	_, _ = db.Exec(` + "`" + `DROP TABLE "ora_users" CASCADE CONSTRAINTS PURGE` + "`" + `)
	for _, st := range stmts {
		if _, err := db.Exec(st); err != nil {
			os.Stderr.WriteString("applying: " + st + "\n" + err.Error() + "\n")
			os.Exit(1)
		}
	}
	// THE WHOLE POINT: a generated package driven through any database/sql
	// driver, reading the port's VALUE shape.
	ex = sqldrv.New(db)
	code := m.Run()
	_, _ = db.Exec(` + "`" + `DROP TABLE "ora_users" CASCADE CONSTRAINTS PURGE` + "`" + `)
	os.Exit(code)
}

func TestInsertSelectUpdateDelete(t *testing.T) {
	ctx := context.Background()
	bal, _ := runtime.ParseDecimal("12345.6789")
	r := &sd.Row{ID: id(1), Email: "ada@example.com", Name: "Ada", Rank: 1,
		Balance: bal, Active: true}
	if err := sd.Insert(ctx, ex, r); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := sd.New().Where(sd.Email.Eq("ada@example.com")).All(ctx, ex, nil)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("select returned %d rows", len(rows))
	}
	got := rows[0]
	if got.Email != "ada@example.com" || got.Name != "Ada" {
		t.Errorf("text came back as %q / %q", got.Email, got.Name)
	}
	// A key past nothing in particular, but through a RAW(16) that the driver
	// hands over as []byte — the one place the value shape has to copy.
	if got.ID != id(1) {
		t.Errorf("the uuid did not round-trip: %v", got.ID)
	}
	// An int8 arrives as a decimal STRING here, so this is a ParseInt rather
	// than a load. If it read zero, valdec's string case is missing.
	if got.Rank != 1 {
		t.Errorf("rank came back as %d", got.Rank)
	}
	// THE MEASUREMENT THAT DECIDED THE DESIGN. A NUMBER(19,4) arrives as
	// "12345.6789" and must survive exactly; through a float64 it would not.
	if got.Balance.String() != "12345.6789" {
		t.Errorf("the decimal did not round-trip: %s", got.Balance.String())
	}
	// go-ora reports a 23c native BOOLEAN as NUMBER and hands back "1", so a
	// decoder that only handled the bool case would read this as false.
	if !got.Active {
		t.Error("the boolean came back false; valdec's string case is missing")
	}

	n, err := sd.New().Count(ctx, ex)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
	ok, err := sd.New().Where(sd.Email.Eq("ada@example.com")).Exists(ctx, ex)
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if !ok {
		t.Error("exists said no about a row that is there")
	}
}

// A NULL must stay distinct from a zero, which is what the whole Oracle
// milestone is about one layer up.
func TestNullIsNotZero(t *testing.T) {
	ctx := context.Background()
	bal, _ := runtime.ParseDecimal("0")
	r := &sd.Row{ID: id(2), Email: "nil@example.com", Name: "Nil", Balance: bal}
	if err := sd.Insert(ctx, ex, r); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := sd.New().Where(sd.Email.Eq("nil@example.com")).All(ctx, ex, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("select: %v (%d rows)", err, len(rows))
	}
	if rows[0].DeletedAt.Valid {
		t.Error("an unset soft-delete column came back as a value")
	}
}

// Paging, which is FETCH FIRST here and not LIMIT.
func TestPaging(t *testing.T) {
	ctx := context.Background()
	bal, _ := runtime.ParseDecimal("1")
	for i := byte(10); i < 15; i++ {
		r := &sd.Row{ID: id(i), Email: string(rune('a'+i)) + "@example.com",
			Name: "n", Rank: int64(i), Balance: bal}
		if err := sd.Insert(ctx, ex, r); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	rows, err := sd.New().OrderByRankAsc().Limit(2).All(ctx, ex, nil)
	if err != nil {
		t.Fatalf("paged read: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("FETCH FIRST returned %d rows, want 2", len(rows))
	}
}

// The soft-delete unique, which is a function-based index on a CASE here —
// the shape MySQL cannot take at all.
func TestSoftDeleteScopedUnique(t *testing.T) {
	ctx := context.Background()
	bal, _ := runtime.ParseDecimal("1")
	a := &sd.Row{ID: id(20), Email: "dup@example.com", Name: "A", Balance: bal}
	if err := sd.Insert(ctx, ex, a); err != nil {
		t.Fatalf("insert: %v", err)
	}
	b := &sd.Row{ID: id(21), Email: "dup@example.com", Name: "B", Balance: bal}
	if err := sd.Insert(ctx, ex, b); err == nil {
		t.Error("two LIVE rows with one email were accepted")
	}
}
`
