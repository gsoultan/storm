package sqldrv

import (
	"context"
	"database/sql"

	"github.com/gsoultan/storm/runtime"
)

// Pool adapts a *sql.DB to runtime.DB — the four port methods, plus StartTx.
//
// It exists because Exec cannot start a transaction. Exec wraps the DB INTERFACE, which
// *sql.DB, *sql.Tx and *sql.Conn all satisfy, and only one of those three can
// start a transaction. Discovering which one you were handed by asserting at
// the call site is exactly the runtime capability sniff ADR-0005 rejected, so
// the distinction is a type instead: hold a Pool when you need to begin, hold
// an Exec when you were given something to run on.
type Pool struct{ DB *sql.DB }

var (
	_ runtime.Executor = Pool{}
	_ runtime.DB       = Pool{}
)

// NewPool returns a runtime.DB over a database/sql pool.
func NewPool(db *sql.DB) Pool { return Pool{DB: db} }

// exec is the Exec view of this pool. A value conversion, not an allocation:
// Exec is one interface field wide and this copies it.
func (p Pool) exec() Exec { return Exec{DB: p.DB} }

func (p Pool) Query(ctx context.Context, query string, args []any) (runtime.Rows, error) {
	return p.exec().Query(ctx, query, args)
}

func (p Pool) Exec(ctx context.Context, query string, args []any) (int64, error) {
	return p.exec().Exec(ctx, query, args)
}

func (p Pool) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	return p.exec().CopyFrom(ctx, table, cols, src)
}

func (p Pool) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {
	return p.exec().Batch(ctx, ops, each)
}

// StartTx starts a transaction with the driver's default isolation level.
//
// The default, and no parameter for it, because an isolation level is a
// property of the transaction the CALLER is designing, not of the adapter: a
// caller who needs SERIALIZABLE has a *sql.DB in hand, calls BeginTx with the
// options they want, and wraps the result with NewTx. This method is the
// common case, not the only door.
func (p Pool) StartTx(ctx context.Context) (runtime.Tx, error) {
	t, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return Tx{T: t}, nil
}
