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

// Conn adapts ONE pinned connection to runtime.Executor.
//
// Pool and Tx cover almost everything, and this covers what they cannot:
// state that lives on a SESSION rather than on a transaction or a statement.
// A session-scoped advisory lock is the case that needs it — pg_advisory_lock
// is held by the connection that took it, so taking it through a pool means
// releasing it on whichever connection the pool hands out next, which releases
// nothing and leaks the lock until that connection is recycled.
//
// C is a connection from Connect, or from Pool.Acquire. The caller owns its
// lifetime: this adapter does not close or release it, because it cannot know
// whether the session state it was pinned for is finished with.
//
// A caduceus connection belongs to one goroutine at a time, and so does a
// Conn: rows read from it hold it until they are closed.
type Conn struct{ C *caduceus.Conn }

func (e Conn) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	return query(ctx, e.C, sql, args, nil)
}

func (e Conn) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	return exec(ctx, e.C, sql, args)
}

func (e Conn) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	return copyFrom(ctx, e.C, table, cols, src)
}

func (e Conn) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	return batch(ctx, e.C, ops, each)
}

// StartTx begins a transaction on the pinned connection.
func (e Conn) StartTx(ctx context.Context) (runtime.Tx, error) {
	t, err := e.C.Begin(ctx)
	if err != nil {
		return nil, classify(err)
	}
	return &Tx{T: t}, nil
}

var _ runtime.DB = Conn{}
