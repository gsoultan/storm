package hooks

import (
	"context"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// executor is the decorator Wrap returns for an Executor that is neither a
// DB nor a Tx, and the part of those two that sends statements.
type executor struct {
	inner runtime.Executor
	h     Hooks
}

// begin runs Before and stamps the start time. An error is Before refusing
// the statement, which is then not sent.
func (e *executor) begin(ctx context.Context, s *Statement) (context.Context, error) {
	if e.h.Before != nil {
		c, err := e.h.Before(ctx, s)
		if err != nil {
			return ctx, err
		}
		if c != nil {
			ctx = c
		}
	}
	s.Start = time.Now()
	return ctx, nil
}

func (e *executor) end(ctx context.Context, s *Statement, n int64, err error) {
	if e.h.After == nil {
		return
	}
	s.Duration = time.Since(s.Start)
	s.Rows = n
	s.Err = err
	e.h.After(ctx, s)
}

func (e *executor) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	s := &Statement{Kind: KindQuery, SQL: sql, Args: args}
	ctx, err := e.begin(ctx, s)
	if err != nil {
		return nil, err
	}
	r, err := e.inner.Query(ctx, sql, args)
	if err != nil {
		e.end(ctx, s, 0, err)
		return nil, err
	}
	if e.h.After == nil {
		return r, nil
	}
	w := &rows{inner: r, e: e, ctx: ctx, s: s}
	// A value-shaped result must stay value-shaped, or the package generated
	// for it fails every read with runtime.ErrByteRows.
	if vr, ok := r.(runtime.ValueRows); ok {
		return &valueRows{rows: w, vals: vr}, nil
	}
	return w, nil
}

func (e *executor) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	s := &Statement{Kind: KindExec, SQL: sql, Args: args}
	ctx, err := e.begin(ctx, s)
	if err != nil {
		return 0, err
	}
	n, err := e.inner.Exec(ctx, sql, args)
	e.end(ctx, s, n, err)
	return n, err
}

func (e *executor) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	s := &Statement{Kind: KindCopy, Table: table, Columns: cols}
	ctx, err := e.begin(ctx, s)
	if err != nil {
		return 0, err
	}
	n, err := e.inner.CopyFrom(ctx, table, cols, src)
	e.end(ctx, s, n, err)
	return n, err
}

func (e *executor) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	s := &Statement{Kind: KindBatch, Ops: ops}
	ctx, err := e.begin(ctx, s)
	if err != nil {
		return err
	}
	err = e.inner.Batch(ctx, ops, each)
	e.end(ctx, s, 0, err)
	return err
}
