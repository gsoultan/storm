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
	"github.com/gsoultan/storm/v2/runtime"
)

// Conn adapts one caduceus connection the caller holds to runtime.Executor.
// A caduceus connection belongs to one goroutine at a time, and so does a
// Conn; Pool is the one to share.
//
// Open the connection with Options, or with Connect, which does.
type Conn struct{ C *caduceus.Conn }

var _ runtime.Executor = Conn{}

func (e Conn) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	return query(ctx, e.C, sql, args, nil)
}

func (e Conn) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	return exec(ctx, e.C, sql, args)
}

// CopyFrom uses the COPY protocol, in binary where the column types allow.
func (e Conn) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	return copyFrom(ctx, e.C, table, cols, src)
}

// Batch sends every statement before reading any result.
func (e Conn) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	return batch(ctx, e.C, ops, each)
}

// StartTx begins a transaction on the connection.
func (e Conn) StartTx(ctx context.Context) (runtime.Tx, error) {
	t, err := e.C.Begin(ctx)
	if err != nil {
		return nil, classify(err)
	}
	return Tx{T: t}, nil
}

var _ runtime.DB = Conn{}
