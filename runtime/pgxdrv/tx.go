package pgxdrv

import (
	"context"
	"errors"

	"github.com/gsoultan/storm/runtime"
	"github.com/jackc/pgx/v5"
)

// Tx adapts a pgx transaction to the Executor port.
//
// This is the other half of ADR-0005's "a transaction is an Executor you were
// given": Begin/Commit/Rollback stay out of the port, the caller owns the
// transaction's lifetime with pgx's own API, and every piece of generated code
// — queries, plans, writes, a Unit flush — runs inside it unchanged, because
// none of them ever knew what was behind the interface.
//
//	tx, err := pool.Begin(ctx)
//	defer tx.Rollback(ctx)
//	ex := pgxdrv.Tx{T: tx}
//	... generated calls against ex ...
//	tx.Commit(ctx)
//
// Without this adapter the doctrine was a sentence, not a capability: Pool was
// the only Executor, so nothing could actually be handed a transaction.
type Tx struct{ T pgx.Tx }

func (e Tx) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	r, err := e.T.Query(ctx, sql, args...)
	if err != nil {
		return nil, classify(err)
	}
	return newRows(r)
}

func (e Tx) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	tag, err := e.T.Exec(ctx, sql, args...)
	if err != nil {
		return 0, classify(err)
	}
	return tag.RowsAffected(), nil
}

func (e Tx) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	n, err := e.T.CopyFrom(ctx, pgx.Identifier{table}, cols, copySrc{src})
	return n, classify(err)
}

func (e Tx) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	if len(ops) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, op := range ops {
		b.Queue(op.SQL, op.Args...)
	}
	return drainBatch(e.T.SendBatch(ctx, b), ops, each)
}

// Commit ends the transaction.
//
// pgx reports a second end as ErrTxClosed; this returns runtime.ErrTxDone
// instead, because a caller holding a runtime.Tx is testing for the shared
// error and has no way to name pgx's.
func (e Tx) Commit(ctx context.Context) error {
	err := e.T.Commit(ctx)
	if errors.Is(err, pgx.ErrTxClosed) {
		return runtime.ErrTxDone
	}
	return classify(err)
}

// Rollback ends the transaction, and returns nil if it has already ended.
//
// The nil is the contract in runtime.Tx and it is the whole reason
// `defer tx.Rollback(ctx)` beside a commit is an idiom rather than a bug: pgx
// answers the rollback that follows a successful commit with ErrTxClosed, and
// a deferred call has nowhere to put an error it was always going to get.
func (e Tx) Rollback(ctx context.Context) error {
	err := e.T.Rollback(ctx)
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return classify(err)
}

var (
	_ runtime.Executor = Tx{}
	_ runtime.Tx       = Tx{}
)
