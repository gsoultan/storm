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
)

// Pool is the SQLite port over a *sql.DB: an Executor that can start a
// transaction.
type Pool struct{ DB *sql.DB }

var (
	_ runtime.Executor = Pool{}
	_ runtime.DB       = Pool{}
)

// NewPool wraps a database handle.
func NewPool(db *sql.DB) Pool { return Pool{DB: db} }

// Query runs a read.
func (p Pool) Query(ctx context.Context, query string, args []any) (runtime.Rows, error) {
	return Exec{DB: p.DB}.Query(ctx, query, args)
}

// Exec runs a write.
func (p Pool) Exec(ctx context.Context, query string, args []any) (int64, error) {
	return Exec{DB: p.DB}.Exec(ctx, query, args)
}

// Batch runs each operation in turn.
func (p Pool) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {
	return Exec{DB: p.DB}.Batch(ctx, ops, each)
}

// CopyFrom loads rows inside a transaction of its own, so a load that fails
// part-way leaves none of its rows behind.
func (p Pool) CopyFrom(ctx context.Context, table string, cols []string,
	src runtime.CopySource) (int64, error) {
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	n, err := copyRows(ctx, tx, table, cols, src)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// StartTx begins a transaction. Whether it takes the write lock at BEGIN is
// the connection's choice — DSN's _txlock=immediate makes it so, and storm's
// lock modes rely on it; see compile/sqlite.
func (p Pool) StartTx(ctx context.Context) (runtime.Tx, error) {
	t, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return Tx{T: t}, nil
}
