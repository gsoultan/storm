package codegen_test

// mariadbLiveSrc runs inside the generated package: real CRUD through storm's
// generated API, over storm's own MySQL adapter, against a real server.
const mariadbLiveSrc = `package PKG_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/myddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/mydrv"
	sd "IMPORTPATH"
)

type mdUser struct {
	storm.Model
	Email     string
	Name      string
	DeletedAt *time.Time
}

func (u *mdUser) Schema(t *storm.Table) {
	t.SoftDelete(&u.DeletedAt)
	t.Col(&u.Email).Size(320)
	t.Col(&u.Name).Size(120)
	// Across deleted rows, not scoped to the live ones: MySQL has no partial
	// index, so the live-scoped form does not port.
	t.UniqueAcrossDeleted(&u.Email)
}

var ex runtime.Executor

func TestMain(m *testing.M) {
	addr := os.Getenv("STORM_MYSQL_ADDR")
	if addr == "" {
		os.Stderr.WriteString("STORM_MYSQL_ADDR did not reach the generated package\n")
		os.Exit(1)
	}
	ctx := context.Background()
	c, err := mydrv.Open(ctx, addr, "root", "storm", "storm")
	must(err)
	defer c.Close()
	ex = c

	s, err := storm.Build(&mdUser{})
	must(err)
	ddl, err := myddl.Create(s)
	must(err)
	_, _ = c.Exec(ctx, "DROP TABLE IF EXISTS " + "` + "`md_users`" + `", nil)
	for _, stmt := range splitDDL(ddl) {
		if _, err := c.Exec(ctx, stmt, nil); err != nil {
			panic(stmt + ": " + err.Error())
		}
	}
	os.Exit(m.Run())
}

func splitDDL(ddl string) []string {
	var out []string
	for _, s := range splitOn(ddl, ';') {
		if t := trim(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func splitOn(s string, sep byte) []string {
	var out []string
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[last:i])
			last = i + 1
		}
	}
	return append(out, s[last:])
}

func trim(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\n' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\n' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func id(n byte) [16]byte {
	var b [16]byte
	b[0], b[15] = n, n
	return b
}

// The whole read/write path through the generated API.
func TestInsertSelectUpdateDelete(t *testing.T) {
	ctx := context.Background()

	r := &sd.Row{ID: id(1), Email: "a@x.com", Name: "Ada"}
	if err := sd.Insert(ctx, ex, r); err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, err := sd.New().Where(sd.Email.Eq("a@x.com")).All(ctx, ex, nil)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "Ada" {
		t.Fatalf("select returned %+v", rows)
	}
	if rows[0].ID != id(1) {
		t.Errorf("the uuid did not round-trip: %v", rows[0].ID)
	}

	n, err := sd.New().Count(ctx, ex)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}

	ok, err := sd.New().Where(sd.Email.Eq("a@x.com")).Exists(ctx, ex)
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if !ok {
		t.Error("exists said no")
	}

	// Soft delete: the row survives, every read stops returning it.
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

	// NOT reusable here: the uniqueness spans deleted rows, because MySQL has
	// no partial index. On PostgreSQL the same model scopes it to the live rows
	// and the address becomes claimable again — a real behavioural difference
	// an adopter has to know about.
	if err := sd.Insert(ctx, ex, &sd.Row{ID: id(2), Email: "a@x.com", Name: "Grace"}); err == nil {
		t.Error("a deleted row's email was reusable; this engine indexes every row")
	}
}

// MariaDB can return the row it wrote, which MySQL 8 cannot — the difference
// that made it the target.
//
// Proved by a value the SERVER changes. storm sends every insertable column
// itself, zero values included, so a plain DEFAULT never fires and RETURNING
// would only echo the input back — which proves nothing about whether the read
// happened.
//
// DATETIME(6) truncates to microseconds. Send nanoseconds, and a row that came
// back from the server has lost them while a row that was never read has not.
func TestInsertReturnsTheRow(t *testing.T) {
	ctx := context.Background()
	withNanos := time.Date(2026, 9, 12, 10, 30, 0, 123456789, time.UTC)
	r := &sd.Row{ID: id(3), Email: "returned@x.com", Name: "Returned", CreatedAt: withNanos}
	if err := sd.Insert(ctx, ex, r); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if r.CreatedAt.Nanosecond() == 123456789 {
		t.Error("created_at still has its nanoseconds — the insert did not read the row back, " +
			"it kept what the caller supplied")
	}
	if got := r.CreatedAt.Nanosecond(); got != 123456000 {
		t.Errorf("created_at nanosecond = %d, want 123456000 (microsecond-truncated by the server)", got)
	}
}
`
