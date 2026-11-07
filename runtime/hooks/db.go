package hooks

import (
	"context"

	"github.com/gsoultan/storm/runtime"
)

// db is a wrapped pool. Its transactions are wrapped with the same hooks, so
// a statement sent inside runtime.InTx is seen like any other.
type db struct {
	executor
	d runtime.DB
}

func (d *db) StartTx(ctx context.Context) (runtime.Tx, error) {
	t, err := d.d.StartTx(ctx)
	if err != nil {
		return nil, err
	}
	return &tx{executor{inner: t, h: d.h}, t}, nil
}
