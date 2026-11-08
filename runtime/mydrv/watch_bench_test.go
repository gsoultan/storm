package mydrv_test

import (
	"context"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime/mydrv"
)

// A request's context has a deadline, so every statement under it arms the
// cancellation watcher. This is the cost of arming it on a statement that
// finishes in time: what context.AfterFunc saves over a goroutine per call.
func BenchmarkPointQueryWithDeadline(b *testing.B) {
	c, err := mydrv.Open(context.Background(), config(b))
	if err != nil {
		b.Skipf("no server: %v", err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		r, err := c.Query(ctx, "SELECT 1", nil)
		if err != nil {
			b.Fatal(err)
		}
		for r.Next() {
		}
		r.Close()
	}
}
