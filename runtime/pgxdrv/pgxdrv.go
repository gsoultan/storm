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
		return nil, err
	}
	return newRows(r)
}

func (e Pool) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	tag, err := e.P.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

type rows struct{ pgx.Rows }

func (r rows) Close() { r.Rows.Close() }

// CopyFrom uses the real COPY protocol, which is the point: it skips statement
// parsing and per-row protocol overhead entirely, so a bulk load is one
// conversation with the server rather than one per row.
func (e Pool) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	return e.P.CopyFrom(ctx, pgx.Identifier{table}, cols, copySrc{src})
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
func drainBatch(br pgx.BatchResults, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	// Close must happen even on an early return: an unclosed batch leaves the
	// connection with unread results and poisons it for the next borrower.
	defer br.Close()

	for i, op := range ops {
		if !op.WantRows {
			tag, err := br.Exec()
			if cbErr := each(i, nil, tag.RowsAffected(), err); cbErr != nil {
				return cbErr
			}
			continue
		}
		r, err := br.Query()
		if err != nil {
			if cbErr := each(i, nil, 0, err); cbErr != nil {
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
		if err := r.Err(); err != nil {
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
