// want: missing method boundToOneShard
//
// Only shard.Set and shard.Pin can produce a Bound. A type that has every
// Executor method and a Shard method still is not one, because Bound carries
// an unexported method — otherwise the compile-time check above would be one
// wrapper away from meaning nothing.
package compilefail

import (
	"context"

	"github.com/gsoultan/storm/examples/tenants/store/order"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/shard"
)

type forged struct{ runtime.Executor }

func (forged) Shard() shard.ID { return 0 }

func OpenOrdersThroughAForgery(ctx context.Context, pool runtime.Executor) error {
	_, err := order.New().All(ctx, forged{pool}, nil)
	return err
}
