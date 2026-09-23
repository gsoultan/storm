package sqldrv

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gsoultan/storm/runtime"
)

// Pool adapts a *sql.DB to runtime.DB — the four port methods, plus Begin.
//
// It exists because Exec cannot have Begin. Exec wraps the DB INTERFACE, which
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

// Begin starts a transaction with the driver's default isolation level.
//
// The default, and no parameter for it, because an isolation level is a
// property of the transaction the CALLER is designing, not of the adapter: a
// caller who needs SERIALIZABLE has a *sql.DB in hand, calls BeginTx with the
// options they want, and wraps the result with NewTx. This method is the
// common case, not the only door.
func (p Pool) Begin(ctx context.Context) (runtime.Tx, error) {
	t, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return Tx{T: t}, nil
}

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
