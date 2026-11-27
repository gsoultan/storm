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
	"errors"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/runtime"
)

// Tx adapts a caduceus transaction to the Executor port.
//
// This is ADR-0005's "a transaction is an Executor you were given":
// Begin/Commit/Rollback stay out of the port, and every piece of generated
// code — queries, plans, writes, a Unit flush — runs inside it unchanged,
// because none of them ever knew what was behind the interface.
//
//	tx, err := pool.StartTx(ctx)
//	defer tx.Rollback(ctx)
//	... generated calls against tx ...
//	tx.Commit(ctx)
//
// A Tx from Pool.StartTx holds its connection until it ends, and gives it back
// to the pool then; one built around a caduceus transaction of the caller's
// leaves the connection to them.
type Tx struct {
	T *caduceus.Tx

	// release gives a pool's connection back once the transaction ends.
	release func()
}

func (e *Tx) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	return query(ctx, e.T, sql, args, nil)
}

func (e *Tx) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	return exec(ctx, e.T, sql, args)
}

func (e *Tx) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	return copyFrom(ctx, e.T, table, cols, src)
}

func (e *Tx) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	return batch(ctx, e.T, ops, each)
}

// Commit ends the transaction.
//
// caduceus reports a second end as ErrTxClosed; this returns runtime.ErrTxDone
// instead, because a caller holding a runtime.Tx is testing for the shared
// error and has no way to name the driver's.
func (e *Tx) Commit(ctx context.Context) error {
	err := e.T.Commit(ctx)
	if errors.Is(err, caduceus.ErrTxClosed) {
		return runtime.ErrTxDone
	}
	e.done()
	return classify(err)
}

// Rollback ends the transaction, and returns nil if it has already ended.
//
// The nil is the contract in runtime.Tx and it is the whole reason
// `defer tx.Rollback(ctx)` beside a commit is an idiom rather than a bug.
func (e *Tx) Rollback(ctx context.Context) error {
	err := e.T.Rollback(ctx)
	if errors.Is(err, caduceus.ErrTxClosed) {
		return nil
	}
	e.done()
	return classify(err)
}

// done hands a pool's connection back, once.
func (e *Tx) done() {
	if e.release != nil {
		release := e.release
		e.release = nil
		release()
	}
}

var (
	_ runtime.Executor = (*Tx)(nil)
	_ runtime.Tx       = (*Tx)(nil)
)
