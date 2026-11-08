package msdrv_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime/msdec"
	"github.com/gsoultan/storm/runtime/msdrv"
)

// A cancelled Exec reports the cancellation. Before, the reply loop ignored
// the DONE token's attention bit, so a statement the server abandoned came
// back as success with whatever count it had reached: a cancelled write the
// caller believed had happened.
func TestCancelledExecReportsTheCancellation(t *testing.T) {
	c := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := c.Exec(ctx, "WAITFOR DELAY '00:00:05'", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled Exec: err = %v, want the caller's deadline", err)
	}
	assertUsable(t, c)
}

// A cancellation that lands as the statement finishes must not leave the
// server's attention acknowledgement on the socket for the NEXT statement to
// read as its answer, nor send an attention into the next statement.
func TestCancellationAtTheFinishLineLeavesTheConnectionClean(t *testing.T) {
	c := open(t)
	for i := range 300 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%40)*50*time.Microsecond)
		_, err := c.Exec(ctx, "SELECT 1", nil)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("round %d: %v", i, err)
		}
		assertUsable(t, c)
	}
}

func assertUsable(t *testing.T, c *msdrv.Conn) {
	t.Helper()
	r, err := c.Query(context.Background(), "SELECT CAST(42 AS INT)", nil)
	if err != nil {
		t.Fatalf("the next statement: %v", err)
	}
	defer r.Close()
	if !r.Next() {
		t.Fatalf("the next statement returned no row: %v", r.Err())
	}
	if v := msdec.Int4(r.RawValues()[0]); v != 42 {
		t.Fatalf("the next statement read %d, not its own answer", v)
	}
	for r.Next() {
	}
	if err := r.Err(); err != nil {
		t.Fatalf("the next statement: %v", err)
	}
}
