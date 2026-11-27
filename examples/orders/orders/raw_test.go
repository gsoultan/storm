// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package orders_test

import (
	"context"
	"testing"

	"github.com/gsoultan/caduceus"
)

// rawQuery runs hand-written SQL on a connection of its own, for a test that
// checks storm's answer against the database's: the Executor port hands back
// raw rows, and these want Scan. The connection is held until the test ends.
func rawQuery(t *testing.T, ctx context.Context, sql string, args ...any) (*caduceus.Rows, error) {
	t.Helper()
	c, release, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	t.Cleanup(release)
	return c.Query(ctx, sql, args...)
}
