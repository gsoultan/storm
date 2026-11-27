// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgdrv_test

import (
	"context"
	"os"
	"testing"

	"github.com/gsoultan/storm/v2/runtime"
	"github.com/gsoultan/storm/v2/runtime/pgdrv"
)

// A session-scoped advisory lock is the thing Pool cannot do.
//
// pg_advisory_lock is held by the CONNECTION that took it, so taking it
// through a pool and releasing it through the pool releases it on whichever
// connection came back next — which releases nothing, and leaks the lock until
// that connection is recycled. This asserts the lock is actually held (a
// second session cannot take it) and actually released.
func TestConnHoldsSessionState(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgdrv.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	held, releaseHeld, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseHeld()
	probe, releaseProbe, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseProbe()

	const key = int64(7_449_001)
	ex := pgdrv.Conn{C: held}
	other := pgdrv.Conn{C: probe}

	rows, err := ex.Query(ctx, "SELECT pg_try_advisory_lock($1)", []any{key})
	if err != nil {
		t.Fatal(err)
	}
	if !scanBool(t, rows) {
		t.Fatal("could not take the lock on a free key")
	}

	// A DIFFERENT session must be refused while it is held.
	rows, err = other.Query(ctx, "SELECT pg_try_advisory_lock($1)", []any{key})
	if err != nil {
		t.Fatal(err)
	}
	if scanBool(t, rows) {
		t.Fatal("a second session took a lock that was already held")
	}

	rows, err = ex.Query(ctx, "SELECT pg_advisory_unlock($1)", []any{key})
	if err != nil {
		t.Fatal(err)
	}
	if !scanBool(t, rows) {
		t.Fatal("unlock reported the lock was not held by this session")
	}

	// And it is genuinely free again, which is what proves the unlock ran on
	// the same session as the lock.
	rows, err = other.Query(ctx, "SELECT pg_try_advisory_lock($1)", []any{key})
	if err != nil {
		t.Fatal(err)
	}
	if !scanBool(t, rows) {
		t.Fatal("the lock was not released")
	}
	if _, err := other.Exec(ctx, "SELECT pg_advisory_unlock($1)", []any{key}); err != nil {
		t.Fatal(err)
	}
}

func scanBool(t *testing.T, rows runtime.Rows) bool {
	t.Helper()
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	v := rows.RawValues()
	if len(v) != 1 || len(v[0]) == 0 {
		t.Fatalf("unexpected row shape %v", v)
	}
	return v[0][0] == 't' || v[0][0] == 1
}

func TestPoolDatabaseSize(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgdrv.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	n, err := pool.DatabaseSize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n <= 0 {
		t.Fatalf("DatabaseSize = %d, want the bytes of a real database", n)
	}
}

// The PgBouncer transaction-pooling setting DEPLOYMENT.md recommends: with no
// statement kept on the server, results must still come back in binary,
// numeric among them, and a statement run twice must still work.
func TestNoStatementCacheStillReadsBinary(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgdrv.NewPoolConfig(ctx, pgdrv.Config{DSN: dsn, Options: pgdrv.Options{StatementCacheCapacity: -1}})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for range 2 {
		rows, err := pool.Query(ctx, "select 1.50::numeric, false", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !rows.Next() {
			t.Fatal(rows.Err())
		}
		d, err := runtime.DecodeNumeric(rows.RawValues()[0])
		if err != nil {
			t.Fatal(err)
		}
		if got := d.String(); got != "1.50" {
			t.Errorf("numeric read as %q, want 1.50", got)
		}
		if b := rows.RawValues()[1]; len(b) != 1 || b[0] != 0 {
			t.Errorf("bool arrived as %q, want the binary zero byte", b)
		}
		rows.Close()
	}
}
