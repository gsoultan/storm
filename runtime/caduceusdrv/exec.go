// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package caduceusdrv

import (
	"context"
	"errors"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/caduceus/wire"
	"github.com/gsoultan/storm/runtime"
)

// query runs sql on s. release, when set, is run once the rows are closed,
// or at once if there are none to close.
func query(ctx context.Context, s session, sql string, in []any, release func()) (runtime.Rows, error) {
	r, err := s.Query(ctx, sql, args(in)...)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, classify(err)
	}
	return newRows(r, release)
}

func exec(ctx context.Context, s session, sql string, in []any) (int64, error) {
	tag, err := s.Exec(ctx, sql, args(in)...)
	if err != nil {
		return 0, classify(err)
	}
	return tag.RowsAffected(), nil
}

func copyFrom(ctx context.Context, s session, table string, cols []string, src runtime.CopySource) (int64, error) {
	n, err := s.CopyFrom(ctx, []string{table}, cols, copySource{src})
	return n, classify(err)
}

// batch sends ops as one batch and hands each result to each, in order. Each
// result is closed before the next is read, and the batch is always closed,
// so the connection is left with nothing unread.
//
// A nil each reports the first error and stops, which is what a caller who
// passed nil means by "did it work".
func batch(ctx context.Context, s session, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) (err error) {
	if len(ops) == 0 {
		return nil
	}
	if each == nil {
		each = func(_ int, _ runtime.Rows, _ int64, err error) error { return err }
	}
	queries := make([]caduceus.Query, len(ops))
	for i, op := range ops {
		queries[i] = caduceus.Query{SQL: op.SQL, Arguments: args(op.Args)}
	}
	br, err := s.SendBatch(ctx, queries)
	if err != nil {
		return classify(err)
	}
	defer func() {
		if cErr := classify(br.Close()); err == nil && cErr != nil {
			err = cErr
		}
	}()
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
		checked, err := newRows(r, nil)
		if err != nil {
			if cbErr := each(i, nil, 0, err); cbErr != nil {
				return cbErr
			}
			continue
		}
		cbErr := each(i, checked, 0, nil)
		checked.Close()
		if cbErr != nil {
			return cbErr
		}
		if err := checked.Err(); err != nil {
			if cbErr := each(i, nil, 0, err); cbErr != nil {
				return cbErr
			}
		}
	}
	return nil
}

// session is what a connection and a transaction both run statements
// through, so Conn, Tx and Pool share one implementation of the port.
type session interface {
	Query(ctx context.Context, sql string, args ...any) (*caduceus.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (caduceus.CommandTag, error)
	SendBatch(ctx context.Context, queries []caduceus.Query) (*caduceus.BatchResults, error)
	CopyFrom(ctx context.Context, table, columns []string, rows caduceus.CopySource) (int64, error)
}

var (
	_ session = (*caduceus.Conn)(nil)
	_ session = (*caduceus.Tx)(nil)
)

// copySource adapts storm's driver-free CopySource to caduceus's, which
// returns each row's error with its values.
type copySource struct{ s runtime.CopySource }

func (c copySource) Next() bool { return c.s.Next() }
func (c copySource) Err() error { return c.s.Err() }

// Values rewrites storm's own types, as every other argument is.
func (c copySource) Values() ([]any, error) { return args(c.s.Values()), c.s.Err() }

// classify turns a server error whose SQLSTATE storm names into a
// *runtime.ConstraintError, and maps caduceus's spent transaction onto
// storm's. Anything else is returned as it is.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var pg *wire.Error
	if !errors.As(err, &pg) {
		return err
	}
	var kind error
	switch pg.SQLState() {
	case "23505":
		kind = runtime.ErrUniqueViolation
	case "23503":
		kind = runtime.ErrForeignKeyViolation
	case "23514":
		kind = runtime.ErrCheckViolation
	case "23502":
		kind = runtime.ErrNotNullViolation
	case "23P01":
		kind = runtime.ErrExclusionViolation
	case "40001":
		kind = runtime.ErrSerializationFailure
	case "40P01":
		kind = runtime.ErrDeadlock
	case "55P03":
		kind = runtime.ErrLockNotAvailable
	default:
		return err
	}
	return &runtime.ConstraintError{
		Kind:       kind,
		Constraint: pg.ConstraintName,
		Table:      pg.TableName,
		Column:     pg.ColumnName,
		Err:        err,
	}
}

// txDone reports whether err says the transaction was already settled.
func txDone(err error) bool { return errors.Is(err, caduceus.ErrTxClosed) }
