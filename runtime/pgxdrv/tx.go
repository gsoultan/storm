package pgxdrv

import (
	"context"

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
		return nil, err
	}
	return newRows(r)
}

func (e Tx) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	tag, err := e.T.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (e Tx) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	return e.T.CopyFrom(ctx, pgx.Identifier{table}, cols, copySrc{src})
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

var _ runtime.Executor = Tx{}
