package codegen_test

// The source of the subprocess test the generated SQL Server package runs.
//
// A template rather than a file, for the reason the MySQL one is: the package
// name is decided by the model and the import path by the temporary directory,
// so there is nothing to compile against until both exist.

const mssqlLiveSrc = `package PKG_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/msddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/msdrv"
	sd "IMPORTPATH"
)

var ex runtime.Executor

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func id(n byte) [16]byte {
	var u [16]byte
	u[15] = n
	return u
}

// The model, restated here so the generated package's own test builds the same
// schema the generator was given. Soft delete with a LIVE-SCOPED unique, which
// is the partial index MySQL has none of.
type msUser struct {
	storm.Model
	Email     string
	Name      string
	Rank      int64
	DeletedAt *time.Time
}

func (u *msUser) Schema(t *storm.Table) {
	t.Col(&u.Email).Size(255)
	t.Col(&u.Name).Size(120)
	t.SoftDelete(&u.DeletedAt)
	t.Unique(&u.Email)
}

func TestMain(m *testing.M) {
	addr := os.Getenv("STORM_MSSQL_ADDR")
	if addr == "" {
		os.Stderr.WriteString("STORM_MSSQL_ADDR did not reach the generated package\n")
		os.Exit(1)
	}
	ctx := context.Background()
	// The server starts empty in CI, so the database has to exist before
	// anything can open it. Two statements, and it removes an out-of-band step
	// whose absence reads as a client defect.
	master, err := msdrv.Open(ctx, msdrv.Config{
		Addr: addr, User: "sa", Password: os.Getenv("STORM_MSSQL_PASSWORD"),
		Database: "master", TLS: msdrv.TLSDisabled,
	})
	must(err)
	_, err = master.Exec(ctx, "IF DB_ID('storm') IS NULL CREATE DATABASE storm", nil)
	must(err)
	master.Close()

	// Through the POOL, because a pool is what an adopter passes and it is the
	// Executor whose concurrency and cancellation the generated code inherits.
	c, err2 := msdrv.NewPool(ctx, msdrv.Config{
		Addr: addr, User: "sa", Password: os.Getenv("STORM_MSSQL_PASSWORD"),
		Database: "storm", TLS: msdrv.TLSDisabled,
	})
	must(err2)
	defer c.Close()
	ex = c

	s, err := storm.Build(&msUser{})
	must(err)
	ddl, err := msddl.Create(s)
	must(err)
	_, _ = c.Exec(ctx, "DROP TABLE IF EXISTS [ms_users]", nil)
	for _, stmt := range strings.Split(ddl, ";") {
		if t := strings.TrimSpace(stmt); t != "" {
			if _, err := c.Exec(ctx, t, nil); err != nil {
				panic(t + ": " + err.Error())
			}
		}
	}
	os.Exit(m.Run())
}

// The round trip, through the generated package rather than around it.
func TestInsertSelectUpdateDelete(t *testing.T) {
	ctx := context.Background()
	r := &sd.Row{ID: id(1), Email: "ada@example.com", Name: "Ada", Rank: 1}
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
	// Text is UTF-16 on this wire. A scanner that let the slab copy the bytes
	// verbatim returns the interleaved-null form — not an error, and not the
	// string either.
	if rows[0].Email != "ada@example.com" || rows[0].Name != "Ada" {
		t.Errorf("text came back as %q / %q", rows[0].Email, rows[0].Name)
	}
	// And a uuid has to be the one that was written, in canonical order: the
	// wire form is mixed-endian, so a client that skips the swap round-trips
	// perfectly and matches nothing anyone else wrote.
	if rows[0].ID != id(1) {
		t.Errorf("the uuid did not round-trip: %v", rows[0].ID)
	}
	if rows[0].Rank != 1 {
		t.Errorf("rank came back as %d", rows[0].Rank)
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

	// Soft delete: the row survives and every read stops returning it.
	if err := sd.Delete(ctx, ex, id(1)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, err = sd.New().All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a deleted row still reads back: %+v", rows)
	}
}

// The insert reads back what the SERVER computed. That is the OUTPUT clause,
// and it is POSITIONAL here — between the column list and VALUES, where every
// other target puts it at the end.
//
// Proved by a value the server changes rather than one storm sent: the
// timestamps are DEFAULT SYSDATETIMEOFFSET(), so a row that was read back has
// them and one that was not is zero.
func TestInsertReturnsTheRow(t *testing.T) {
	ctx := context.Background()
	// Proved by a value the SERVER changes, not one it echoes. storm sends
	// every insertable column itself, zero values included, so a plain DEFAULT
	// never fires and a returning clause that only echoed the input back would
	// prove nothing about whether the read happened.
	//
	// datetimeoffset(7) resolves to a hundred nanoseconds. Send nanoseconds,
	// and a row that came back from the server has lost them.
	withNanos := time.Date(2026, 9, 20, 10, 30, 0, 123456789, time.UTC)
	r := &sd.Row{ID: id(3), Email: "returned@example.com", Name: "Returned",
		Rank: 3, CreatedAt: withNanos}
	if err := sd.Insert(ctx, ex, r); err != nil {
		t.Fatal(err)
	}
	if r.CreatedAt.Nanosecond() == 123456789 {
		t.Error("created_at still has its nanoseconds — the insert did not read the " +
			"row back, it kept what the caller supplied")
	}
	if got := r.CreatedAt.Nanosecond(); got != 123456700 {
		t.Errorf("created_at came back with %d nanoseconds, want 123456700", got)
	}
	// Cleared, because the paging test below counts on knowing every live row.
	// Leaving it made that test report a duplicate rank as a paging defect —
	// which is a shared fixture telling a lie about the code under test.
	if err := sd.Delete(ctx, ex, id(3)); err != nil {
		t.Fatal(err)
	}
}

// The model MySQL cannot take: uniqueness scoped to the LIVE rows, which is a
// partial index. myddl.Check refuses it; msddl emits a filtered index.
func TestSoftDeleteScopedUnique(t *testing.T) {
	ctx := context.Background()
	a := &sd.Row{ID: id(10), Email: "dup@example.com", Name: "First", Rank: 1}
	if err := sd.Insert(ctx, ex, a); err != nil {
		t.Fatal(err)
	}
	b := &sd.Row{ID: id(11), Email: "dup@example.com", Name: "Second", Rank: 2}
	if err := sd.Insert(ctx, ex, b); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Fatalf("a second LIVE row with the same email was accepted: %v", err)
	}

	// Delete the first and the email is free again, which is the whole point of
	// scoping the index to the live rows.
	if err := sd.Delete(ctx, ex, id(10)); err != nil {
		t.Fatal(err)
	}
	c := &sd.Row{ID: id(12), Email: "dup@example.com", Name: "Third", Rank: 3}
	if err := sd.Insert(ctx, ex, c); err != nil {
		t.Fatalf("the deleted row kept its email reserved: %v", err)
	}
	if err := sd.Delete(ctx, ex, id(12)); err != nil {
		t.Fatal(err)
	}
}

// Paging, which is a clause OF the ordering here — so a capped read with no
// ordering is a syntax error without the fallback — and whose operands are
// REVERSED against LIMIT/OFFSET, so this is also where the binder is proved to
// agree with the text.
func TestPagingAndKeyset(t *testing.T) {
	ctx := context.Background()
	for i := 1; i <= 6; i++ {
		r := &sd.Row{ID: id(byte(20 + i)), Email: "u" + string(rune('0'+i)) + "@example.com",
			Name: "User", Rank: int64(i)}
		if err := sd.Insert(ctx, ex, r); err != nil {
			t.Fatal(err)
		}
	}

	// No ordering at all.
	rows, err := sd.New().Limit(3).All(ctx, ex, nil)
	if err != nil {
		t.Fatalf("an unordered capped read: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("Limit(3) returned %d rows", len(rows))
	}

	first, err := sd.New().Order(sd.Rank.Asc()).Limit(2).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sd.New().Order(sd.Rank.Asc()).Limit(2).Offset(2).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("paging returned %d and %d rows", len(first), len(second))
	}
	if first[0].Rank != 1 || first[1].Rank != 2 {
		t.Errorf("the first page is %d,%d", first[0].Rank, first[1].Rank)
	}
	if second[0].Rank != 3 || second[1].Rank != 4 {
		t.Errorf("OFFSET bound the wrong argument: the second page is %d,%d",
			second[0].Rank, second[1].Rank)
	}
	for i := 1; i <= 6; i++ {
		_ = sd.Delete(ctx, ex, id(byte(20+i)))
	}
}

// Row locks, which are a TABLE HINT here rather than a trailing clause — so
// they change the statement's PREFIX, which is the one place the seam had no
// room for them before M10.
func TestLocking(t *testing.T) {
	ctx := context.Background()
	r := &sd.Row{ID: id(40), Email: "lock@example.com", Name: "Locked", Rank: 9}
	if err := sd.Insert(ctx, ex, r); err != nil {
		t.Fatal(err)
	}
	p, ok := ex.(*msdrv.Pool)
	if !ok {
		t.Skip("the executor is not a pool")
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())

	rows, err := sd.New().Where(sd.Email.Eq("lock@example.com")).ForUpdate().All(ctx, tx, nil)
	if err != nil {
		t.Fatalf("a locking read: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("the locking read returned %d rows", len(rows))
	}
}
`
