package shard

import (
	"context"

	"github.com/gsoultan/storm/runtime"
)

// BoundTx is a transaction on one shard.
//
// There is no cross-shard BoundTx and there will not be one: see the package
// note, and Unit for what storm does when it can see a write crossing.
type BoundTx interface {
	Bound

	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// bound is the ordinary Bound: an Executor plus the shard it belongs to.

type boundTx struct {
	bound
	tx runtime.Tx
}

var _ BoundTx = boundTx{}

func (b boundTx) Commit(ctx context.Context) error   { return b.tx.Commit(ctx) }
func (b boundTx) Rollback(ctx context.Context) error { return b.tx.Rollback(ctx) }
