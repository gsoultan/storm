// Package pgxdrv is the only package in storm that knows pgx exists.
// No pgx type crosses out of it (AGENTS.md, CI-enforced).
package pgxdrv

import (
	"context"

	"github.com/gsoultan/storm/runtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool adapts a pgxpool to runtime.Executor.
type Pool struct{ P *pgxpool.Pool }

func (e Pool) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	r, err := e.P.Query(ctx, sql, args...)
	if err != nil {
		return nil, classify(err)
	}
	return newRows(r)
}

func (e Pool) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	tag, err := e.P.Exec(ctx, sql, args...)
	if err != nil {
		return 0, classify(err)
	}
	return tag.RowsAffected(), nil
}

type rows struct{ pgx.Rows }

func (r rows) Close() { r.Rows.Close() }

// Err classifies too. pgx DEFERS a statement's error to here — a constraint
// violation on a RETURNING insert arrives when the rows are drained, not when
// Query is called — so classifying only the immediate return would miss the
// write path entirely, which is the path constraints are on.
func (r rows) Err() error { return classify(r.Rows.Err()) }

// CopyFrom uses the real COPY protocol, which is the point: it skips statement
// parsing and per-row protocol overhead entirely, so a bulk load is one
// conversation with the server rather than one per row.
func (e Pool) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	n, err := e.P.CopyFrom(ctx, pgx.Identifier{table}, cols, copySrc{src})
	return n, classify(err)
}

// copySrc adapts storm's driver-free CopySource to pgx's. The two have the same
// shape on purpose; this exists so that pgx.CopyFromSource does not have to be
// named anywhere outside this package.
type copySrc struct{ s runtime.CopySource }

func (c copySrc) Next() bool             { return c.s.Next() }
func (c copySrc) Values() ([]any, error) { return c.s.Values(), c.s.Err() }
func (c copySrc) Err() error             { return c.s.Err() }

// Batch pipelines every statement before reading any result, so N statements
// cost one round trip.
//
// Results are consumed strictly in order and each is closed before the next is
// requested — pgx invalidates the previous result when the batch advances, so
// holding one across the loop would read freed memory.
func (e Pool) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	if len(ops) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, op := range ops {
		b.Queue(op.SQL, op.Args...)
	}
	return drainBatch(e.P.SendBatch(ctx, b), ops, each)
}

// drainBatch walks a batch's results in order, shared by Pool and Tx — the
// discipline (consume strictly in order, close each result before requesting
// the next, always Close the batch) is identical whoever sent it.
//
// A nil `each` means the caller only wants to know whether the batch
// succeeded, which a bulk insert or upsert usually does. It used to be a nil
// function CALL — the first statement's result took the connection down with
// it, and the symptom was the whole test binary hanging rather than an error
// naming the batch. The callback is now normalised once, here, so no branch
// below has to remember.
func drainBatch(br pgx.BatchResults, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	// Close must happen even on an early return: an unclosed batch leaves the
	// connection with unread results and poisons it for the next borrower.
	defer br.Close()

	if each == nil {
		// The results still have to be drained in order — skipping them
		// desyncs the connection — so the default reports the first error and
		// stops, which is what a caller who passed nil means by "did it work".
		each = func(_ int, _ runtime.Rows, _ int64, err error) error { return err }
	}

	for i, op := range ops {
		if !op.WantRows {
			tag, err := br.Exec()
			if cbErr := each(i, nil, tag.RowsAffected(), classify(err)); cbErr != nil {
				return cbErr
			}
			continue
		}
		r, err := br.Query()
		if err != nil {
			if cbErr := each(i, nil, 0, classify(err)); cbErr != nil {
				return cbErr
			}
			continue
		}
		checked, cErr := newRows(r)
		if cErr != nil {
			// newRows closed the result already.
			if cbErr := each(i, nil, 0, cErr); cbErr != nil {
				return cbErr
			}
			continue
		}
		cbErr := each(i, checked, 0, nil)
		r.Close() // invalidates r; nothing may hold it past here
		if cbErr != nil {
			return cbErr
		}
		if err := classify(r.Err()); err != nil {
			if cbErr := each(i, nil, 0, err); cbErr != nil {
				return cbErr
			}
		}
	}
	return nil
}

var _ runtime.Executor = Pool{}

// NewPool builds a pool with storm's fast parameter encoders installed.
//
// It is a thin wrapper: everything else about the pool stays the caller's.
// An application that configures its own pool should call RegisterFastArrays
// from AfterConnect instead, which is all this does.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return NewPoolConfig(ctx, cfg)
}

// NewPoolConfig is NewPool over a config the caller has already tuned.
func NewPoolConfig(ctx context.Context, cfg *pgxpool.Config) (*pgxpool.Pool, error) {
	if err := refuseTextModes(cfg.ConnConfig.DefaultQueryExecMode); err != nil {
		return nil, err
	}
	prev := cfg.AfterConnect
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		RegisterFastArrays(c.TypeMap())
		if prev != nil {
			return prev(ctx, c)
		}
		return nil
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}
