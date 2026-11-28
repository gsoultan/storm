// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlitedrv

import (
	"context"
	"database/sql"

	"github.com/gsoultan/storm/v2/runtime"
	"github.com/gsoultan/storm/v2/runtime/sqldrv"
)

// Tx is the SQLite port over a *sql.Tx.
type Tx struct{ T *sql.Tx }

var (
	_ runtime.Executor = Tx{}
	_ runtime.Tx       = Tx{}
)

// Query runs a read.
func (t Tx) Query(ctx context.Context, query string, args []any) (runtime.Rows, error) {
	return Exec{DB: t.T}.Query(ctx, query, args)
}

// Exec runs a write.
func (t Tx) Exec(ctx context.Context, query string, args []any) (int64, error) {
	return Exec{DB: t.T}.Exec(ctx, query, args)
}

// Batch runs each operation in turn.
func (t Tx) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {
	return Exec{DB: t.T}.Batch(ctx, ops, each)
}

// CopyFrom loads rows in this transaction.
func (t Tx) CopyFrom(ctx context.Context, table string, cols []string,
	src runtime.CopySource) (int64, error) {
	return copyRows(ctx, t.T, table, cols, src)
}

// Commit ends the transaction, keeping its writes.
func (t Tx) Commit(ctx context.Context) error { return sqldrv.Tx{T: t.T}.Commit(ctx) }

// Rollback ends the transaction, discarding its writes. Rolling back a
// transaction that already ended is not an error.
func (t Tx) Rollback(ctx context.Context) error { return sqldrv.Tx{T: t.T}.Rollback(ctx) }
