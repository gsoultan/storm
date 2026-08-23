package bench

import (
	"context"
	"fmt"
	"testing"

	"github.com/gsoultan/raorm/bench/genuser"
	"github.com/gsoultan/raorm/runtime/pgxdrv"
)

// Greatest-n-per-group: which lowering is the default is decided here, not in
// an argument. Both are generated from the same model, so this compares two
// lowerings and not two hand-written approximations.
//
// The shape of the answer is expected to depend on the ratio of children kept
// to children scanned: LATERAL can stop each parent's scan at n rows, while the
// window form numbers every matching child and discards the rest. So both
// parent count and n are varied — a single data point would pick a default that
// is wrong for half the callers.
func BenchmarkTopNPerParent(b *testing.B) {
	ex := pgxdrv.Pool{P: pool}
	ctx := context.Background()
	order := []genuser.Sort{genuser.CreatedAt.Desc(), genuser.ID.Asc()}

	for _, parents := range []int{1, 10, 100} {
		for _, n := range []int64{1, 5, 50} {
			ids := make([][16]byte, parents)
			for i := range ids {
				ids[i] = orgs[i%len(orgs)]
			}
			label := fmt.Sprintf("parents=%d/n=%d", parents, n)

			b.Run(label+"/window", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := genuser.BatchTopByOrgIDWindow(ctx, ex, ids, n, order...); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(label+"/lateral", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := genuser.BatchTopByOrgIDLateral(ctx, ex, ids, n, order...); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
