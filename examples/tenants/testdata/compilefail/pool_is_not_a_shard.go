// want: does not implement shard.Bound
//
// The headline claim of sharding, in one file. A sharded model's reads take a
// shard.Bound, so "open orders" asked of a pool — which is four databases'
// worth of answers — does not compile. A query sent to the wrong shard fails at
// nothing at run time, so this is the only place the check can live.
package compilefail

import (
	"context"

	"github.com/gsoultan/storm/examples/tenants/store/order"
	"github.com/gsoultan/storm/runtime"
)

func OpenOrdersFromAPool(ctx context.Context, pool runtime.Executor) error {
	_, err := order.New().All(ctx, pool, nil)
	return err
}
