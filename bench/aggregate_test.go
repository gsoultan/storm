package bench

import (
	"context"
	"testing"

	"github.com/gsoultan/storm/bench/genuser"
	"github.com/gsoultan/storm/runtime"
)

// The claim an aggregation has to earn is the one every other read makes: a
// warm shape allocates NOTHING to build its SQL and bind its arguments.
// Aggregations added a second statement cache and a longer suffix, and either
// could have leaked an allocation onto the warm path.
//
// Measured through the real terminal against an executor that returns no rows,
// so what is counted is storm's build-and-bind and not pgx's I/O.

type emptyRows struct{}

func (emptyRows) Next() bool          { return false }
func (emptyRows) RawValues() [][]byte { return nil }
func (emptyRows) Close()              {}
func (emptyRows) Err() error          { return nil }

// nullExec answers every query with zero rows. It allocates nothing itself, so
// anything the benchmark counts belongs to storm.
type nullExec struct{}

func (nullExec) Query(context.Context, string, []any) (runtime.Rows, error) {
	return emptyRows{}, nil
}
func (nullExec) Exec(context.Context, string, []any) (int64, error) { return 0, nil }
func (nullExec) CopyFrom(context.Context, string, []string, runtime.CopySource) (int64, error) {
	return 0, nil
}
func (nullExec) Batch(context.Context, []runtime.BatchOp,
	func(int, runtime.Rows, int64, error) error) error {
	return nil
}

func BenchmarkAggregate_BuildAndBind_Warm(b *testing.B) {
	ctx := context.Background()
	ex := nullExec{}
	q := genuser.New().StatusEq("active")
	dst := make([]genuser.ByStatusRow, 0, 8)
	var sl runtime.Slab
	if _, err := q.AllByStatusInto(ctx, ex, dst[:0], &sl); err != nil { // warm the shape
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := q.AllByStatusInto(ctx, ex, dst[:0], &sl); err != nil {
			b.Fatal(err)
		}
	}
}

// An assertion, not a report: a regression here is a correctness failure
// against docs/PERFORMANCE.md, not a number that drifted.
func TestAggregateWarmPathAllocatesNothing(t *testing.T) {
	ctx := context.Background()
	ex := nullExec{}
	q := genuser.New().StatusEq("active")
	dst := make([]genuser.ByStatusRow, 0, 8)
	var sl runtime.Slab
	if _, err := q.AllByStatusInto(ctx, ex, dst[:0], &sl); err != nil {
		t.Fatal(err)
	}

	got := testing.AllocsPerRun(200, func() {
		if _, err := q.AllByStatusInto(ctx, ex, dst[:0], &sl); err != nil {
			t.Fatal(err)
		}
	})
	if got != 0 {
		t.Errorf("a warm aggregation allocates %.0f time(s) to build and bind; the budget is 0", got)
	}
}
