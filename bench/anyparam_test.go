package bench

import (
	"context"
	"strconv"
	"testing"

	"github.com/gsoultan/raorm/bench/genuser"
	"github.com/gsoultan/raorm/runtime/pgxdrv"
)

// The `= ANY($1)` parameter side is where a relation load's cost now lives:
// every generated plan batches children with it, so an allocation per bound id
// is an allocation per child row's parent.
//
// This measures the PARAMETER side specifically — the scan side is already at a
// handful of allocations per query — by holding the result set to one row while
// varying the number of ids bound.
func BenchmarkAnyParam(b *testing.B) {
	ex := pgxdrv.Pool{P: pool}
	ctx := context.Background()

	for _, n := range []int{1, 50, 500} {
		ids := make([][16]byte, n)
		copy(ids, orgs)
		for i := range ids {
			ids[i] = orgs[i%len(orgs)]
		}
		b.Run(name(n), func(b *testing.B) {
			dst := make([]genuser.Row, 0, 1)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var err error
				dst, err = genuser.New().
					Where(genuser.OrgID.In(ids...)).
					Limit(1).
					All(ctx, ex, dst[:0])
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func name(n int) string { return strconv.Itoa(n) + "_ids" }
