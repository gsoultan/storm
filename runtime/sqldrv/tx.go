package sqldrv

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gsoultan/storm/runtime"
)

// Tx adapts a *sql.Tx to runtime.Tx.
//
// Every statement inside it runs on the transaction's pinned connection,
// which is database/sql's guarantee rather than this adapter's — and is the
// reason CopyFrom's emulated multi-row insert is safe here: the rows land in
// the same transaction they were started in.
type Tx struct{ T *sql.Tx }

var (
	_ runtime.Executor = Tx{}
	_ runtime.Tx       = Tx{}
)

// NewTx wraps a transaction the caller started themselves — the door for a
// BeginTx with explicit *sql.TxOptions.
func NewTx(t *sql.Tx) Tx { return Tx{T: t} }

func (t Tx) exec() Exec { return Exec{DB: t.T} }

func (t Tx) Query(ctx context.Context, query string, args []any) (runtime.Rows, error) {
	return t.exec().Query(ctx, query, args)
}

func (t Tx) Exec(ctx context.Context, query string, args []any) (int64, error) {
	return t.exec().Exec(ctx, query, args)
}

func (t Tx) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	return t.exec().CopyFrom(ctx, table, cols, src)
}

func (t Tx) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {
	return t.exec().Batch(ctx, ops, each)
}

// Commit ends the transaction. A second commit is runtime.ErrTxDone.
//
// The ctx is accepted and not used, because *sql.Tx.Commit does not take one:
// database/sql binds the transaction's cancellation to the context given to
// BeginTx, at the start, and there is no way to hand it a second one at the
// end. Dropping the parameter instead would mean this type could not satisfy
// runtime.Tx, which is the point of it.
func (t Tx) Commit(context.Context) error {
	if err := t.T.Commit(); err != nil {
		if errors.Is(err, sql.ErrTxDone) {
			return runtime.ErrTxDone
		}
		return err
	}
	return nil
}

// Rollback ends the transaction, and returns nil if it has already ended —
// the runtime.Tx contract, so `defer tx.Rollback(ctx)` beside a commit is the
// idiom. database/sql reports the second end as sql.ErrTxDone.
func (t Tx) Rollback(context.Context) error {
	if err := t.T.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return err
	}
	return nil
}
