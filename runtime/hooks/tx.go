package hooks

import (
	"context"

	"github.com/gsoultan/storm/runtime"
)

// tx is a wrapped transaction. Commit and Rollback pass straight through:
// they are not statements a generated package sends.
type tx struct {
	executor
	t runtime.Tx
}

func (t *tx) Commit(ctx context.Context) error   { return t.t.Commit(ctx) }
func (t *tx) Rollback(ctx context.Context) error { return t.t.Rollback(ctx) }
