package mydrv_test

import (
	"context"
	"os"
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/mydrv"
)

// The port, satisfied at compile time. A driver that implements three of four
// methods is not a driver, and the compiler is the cheapest place to learn it.
var _ runtime.Executor = (*mydrv.Conn)(nil)

func addr(t testing.TB) string {
	a := os.Getenv("STORM_MYSQL_ADDR")
	if a == "" {
		t.Skip("STORM_MYSQL_ADDR unset")
	}
	return a
}

func open(t testing.TB) *mydrv.Conn {
	t.Helper()
	c, err := mydrv.Open(context.Background(), addr(t), "root", "storm", "storm")
	if err != nil {
		t.Skipf("no server: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestQueryReturnsRawBytes(t *testing.T) {
	c := open(t)
	ctx := context.Background()
	mustExec(t, c, "DROP TABLE IF EXISTS `port_probe`")
	mustExec(t, c, "CREATE TABLE `port_probe` (`id` BIGINT PRIMARY KEY, `s` VARCHAR(40))")
	for i := 1; i <= 3; i++ {
		if _, err := c.Exec(ctx, "INSERT INTO `port_probe` VALUES (?, ?)",
			[]any{int64(i * 1000), "row"}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := c.Query(ctx, "SELECT `id`,`s` FROM `port_probe` WHERE `id` > ? ORDER BY `id`",
		[]any{int64(500)})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	n := 0
	for r.Next() {
		v := r.RawValues()
		if len(v) != 2 {
			t.Fatalf("row has %d columns, want 2", len(v))
		}
		n++
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("read %d rows, want 3", n)
	}
}

// Exec must report what it affected — a write path that always says zero is a
// write path that cannot tell a missing row from a successful one, which is how
// storm's Delete distinguishes ErrNoRow.
func TestExecReportsAffectedRows(t *testing.T) {
	c := open(t)
	ctx := context.Background()
	mustExec(t, c, "DROP TABLE IF EXISTS `aff_probe`")
	mustExec(t, c, "CREATE TABLE `aff_probe` (`id` BIGINT PRIMARY KEY)")
	for i := 1; i <= 3; i++ {
		if _, err := c.Exec(ctx, "INSERT INTO `aff_probe` VALUES (?)", []any{int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := c.Exec(ctx, "DELETE FROM `aff_probe` WHERE `id` > ?", []any{int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("affected = %d, want 2", n)
	}
	n, err = c.Exec(ctx, "DELETE FROM `aff_probe` WHERE `id` = ?", []any{int64(99)})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("affected = %d for a row that is not there, want 0", n)
	}
}

func mustExec(t *testing.T, c *mydrv.Conn, sql string) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql, nil); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
