// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgdrv

import (
	"context"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/runtime"
)

// query runs a statement and wraps its rows; done runs when they close, or
// here if there are none to close.
func query(ctx context.Context, s session, sql string, args []any, done func()) (runtime.Rows, error) {
	r, err := s.Query(ctx, sql, params(args)...)
	if err != nil {
		if done != nil {
			done()
		}
		return nil, classify(err)
	}
	return newRows(r, done)
}

func exec(ctx context.Context, s session, sql string, args []any) (int64, error) {
	tag, err := s.Exec(ctx, sql, params(args)...)
	if err != nil {
		return 0, classify(err)
	}
	return tag.RowsAffected(), nil
}

// copyFrom uses the real COPY protocol, which is the point: it skips statement
// parsing and per-row protocol overhead, so a bulk load is one conversation
// with the server rather than one per row.
func copyFrom(ctx context.Context, s session, table string, cols []string, src runtime.CopySource) (int64, error) {
	n, err := s.CopyFrom(ctx, []string{table}, cols, copySrc{src})
	return n, classify(err)
}

// copySrc adapts storm's driver-free CopySource to caduceus's.
type copySrc struct{ s runtime.CopySource }

func (c copySrc) Next() bool             { return c.s.Next() }
func (c copySrc) Values() ([]any, error) { return params(c.s.Values()), c.s.Err() }
func (c copySrc) Err() error             { return c.s.Err() }

// batch sends every statement before reading any result, so N statements
// cost one round trip, and walks the results strictly in order.
//
// A nil `each` means the caller only wants to know whether the batch
// succeeded, which a bulk insert or upsert usually does: the results are still
// drained in order, and the first error is returned.
func batch(ctx context.Context, s session, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	if len(ops) == 0 {
		return nil
	}
	queries := make([]caduceus.Query, len(ops))
	for i, op := range ops {
		queries[i] = caduceus.Query{SQL: op.SQL, Arguments: params(op.Args)}
	}
	br, err := s.SendBatch(ctx, queries)
	if err != nil {
		return classify(err)
	}
	// Close must happen even on an early return: an unclosed batch leaves the
	// connection with unread results and poisons it for the next borrower.
	defer br.Close()

	if each == nil {
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
		checked, cErr := newRows(r, nil)
		if cErr != nil {
			if cbErr := each(i, nil, 0, cErr); cbErr != nil {
				return cbErr
			}
			continue
		}
		cbErr := each(i, checked, 0, nil)
		r.Close() // reading on invalidates r; nothing may hold it past here
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
