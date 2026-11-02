package mgdrv

import (
	"context"
	"errors"

	"github.com/gsoultan/storm/runtime"
)

// tx is a transaction on a pinned connection.
//
// MongoDB transactions are per SESSION, and a connection here owns exactly
// one, so the transaction is the connection's session at its next
// transaction number. The transaction starts with its first statement, which
// carries startTransaction; a transaction that never ran one commits and rolls
// back without a round trip.
type tx struct {
	p       *Pool
	c       *conn
	started bool
	done    bool
}

var _ runtime.Tx = (*tx)(nil)

// fields writes the session and transaction into a command.
func (t *tx) fields(b []byte) []byte {
	b = appendName(b, tDocument, "lsid")
	b, at := startDoc(b)
	b = appendBinary(b, "id", 4, t.c.lsid[:])
	b = endDoc(b, at)
	b = appendInt64(b, "txnNumber", t.c.txnNum)
	b = appendBool(b, "autocommit", false)
	if !t.started {
		t.started = true
		b = appendBool(b, "startTransaction", true)
	}
	return b
}

func (t *tx) Query(ctx context.Context, cmd string, args []any) (runtime.Rows, error) {
	if t.done {
		return nil, runtime.ErrTxDone
	}
	return query(ctx, t.c, cmd, args, t.fields, nil)
}

func (t *tx) Exec(ctx context.Context, cmd string, args []any) (int64, error) {
	if t.done {
		return 0, runtime.ErrTxDone
	}
	return exec(ctx, t.c, cmd, args, t.fields)
}

func (t *tx) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {
	if t.done {
		return runtime.ErrTxDone
	}
	return batch(ctx, t, ops, each)
}

func (t *tx) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	if t.done {
		return 0, runtime.ErrTxDone
	}
	return copyFrom(ctx, t.c, table, cols, src, t.fields)
}

func (t *tx) Commit(ctx context.Context) error { return t.end(ctx, "commitTransaction") }

func (t *tx) Rollback(ctx context.Context) error { return t.end(ctx, "abortTransaction") }

func (t *tx) end(ctx context.Context, verb string) error {
	if t.done {
		return runtime.ErrTxDone
	}
	t.done = true
	defer t.p.release(t.c)
	if !t.started {
		return nil
	}
	_, err := t.c.run(ctx, func(b []byte) []byte {
		b = appendInt32(b, verb, 1)
		b = appendName(b, tDocument, "lsid")
		b, at := startDoc(b)
		b = appendBinary(b, "id", 4, t.c.lsid[:])
		b = endDoc(b, at)
		b = appendInt64(b, "txnNumber", t.c.txnNum)
		b = appendBool(b, "autocommit", false)
		return appendStr(b, "$db", "admin")
	})
	if err != nil && verb == "abortTransaction" {
		// The transaction is gone either way — aborted, or never started on
		// the server because its first statement failed. Report what the
		// caller can act on: nothing.
		var e *Error
		if errors.As(err, &e) && e.Code == codeNoSuchTransaction {
			return nil
		}
	}
	return err
}
