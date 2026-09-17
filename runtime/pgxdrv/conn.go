package pgxdrv

import (
	"context"

	"github.com/gsoultan/storm/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PgxConn is the slice of a single pgx connection this adapter needs.
//
// An interface rather than a concrete type because the two things an adopter
// pins are different types with the same shape: *pgx.Conn, and the
// *pgxpool.Conn handed back by pool.Acquire.
type PgxConn interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	CopyFrom(ctx context.Context, table pgx.Identifier, cols []string, src pgx.CopyFromSource) (int64, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Conn adapts ONE pinned connection to runtime.Executor.
//
// Pool and Tx cover almost everything, and this covers what they cannot:
// state that lives on a SESSION rather than on a transaction or a statement.
// A session-scoped advisory lock is the case that needs it — pg_advisory_lock
// is held by the connection that took it, so taking it through a pool means
// releasing it on whichever connection the pool hands out next, which releases
// nothing and leaks the lock until that connection is recycled.
//
// The caller owns the connection's lifetime. This adapter does not release it,
// because it cannot know whether the session state it was pinned for is
// finished with.
type Conn struct{ C PgxConn }

func (e Conn) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	r, err := e.C.Query(ctx, sql, args...)
	if err != nil {
		return nil, classify(err)
	}
	return newRows(r)
}

func (e Conn) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	tag, err := e.C.Exec(ctx, sql, args...)
	if err != nil {
		return 0, classify(err)
	}
	return tag.RowsAffected(), nil
}

func (e Conn) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	n, err := e.C.CopyFrom(ctx, pgx.Identifier{table}, cols, copySrc{src})
	return n, classify(err)
}

func (e Conn) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	if len(ops) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, op := range ops {
		b.Queue(op.SQL, op.Args...)
	}
	return drainBatch(e.C.SendBatch(ctx, b), ops, each)
}
