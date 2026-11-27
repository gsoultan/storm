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

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/runtime"
)

// Tx adapts a caduceus transaction to runtime.Tx. done, when set, is run
// once the transaction is settled: a Pool's transaction gives its
// connection back there.
type Tx struct {
	T    *caduceus.Tx
	done func()
}

var (
	_ runtime.Executor = Tx{}
	_ runtime.Tx       = Tx{}
)

func (e Tx) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	return query(ctx, e.T, sql, args, nil)
}

func (e Tx) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	return exec(ctx, e.T, sql, args)
}

func (e Tx) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	return copyFrom(ctx, e.T, table, cols, src)
}

func (e Tx) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	return batch(ctx, e.T, ops, each)
}

// Commit commits, and returns runtime.ErrTxDone for a transaction already
// settled, as runtime.Tx says.
func (e Tx) Commit(ctx context.Context) error {
	err := e.T.Commit(ctx)
	if txDone(err) {
		return runtime.ErrTxDone
	}
	e.settled()
	return classify(err)
}

// Rollback rolls back, and is nil for a transaction already settled, so it
// can be deferred.
func (e Tx) Rollback(ctx context.Context) error {
	err := e.T.Rollback(ctx)
	if txDone(err) {
		return nil
	}
	e.settled()
	return classify(err)
}

func (e Tx) settled() {
	if e.done != nil {
		e.done()
	}
}
