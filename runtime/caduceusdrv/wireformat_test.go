// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package caduceusdrv_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/v2/runtime"
	"github.com/gsoultan/storm/v2/runtime/caduceusdrv"
)

// The hazard this guards: generated scanners decode RAW wire bytes as binary,
// and a caduceus connection opened without storm's options asks for numeric,
// inet, cidr and tstzrange in text. The executor refuses that result rather
// than handing back a decimal decoded from ASCII digits.
func TestWireFormat_ExecutorRefusesTextResults(t *testing.T) {
	ctx := context.Background()
	raw, err := caduceus.Connect(ctx, dsn(t), caduceus.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close(ctx)
	ex := caduceusdrv.Conn{C: raw}

	_, err = ex.Query(ctx, `SELECT 1.5::numeric AS price`, nil)
	if err == nil {
		t.Fatal("a text-format result must be refused, not decoded")
	}
	for _, want := range []string{`column 1 "price"`, "text format", "caduceusdrv.Connect"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}

	// Batch takes the same path.
	berr := ex.Batch(ctx, []runtime.BatchOp{{SQL: `SELECT 1.5::numeric AS price`, WantRows: true}},
		func(_ int, _ runtime.Rows, _ int64, err error) error { return err })
	if berr == nil {
		t.Fatal("batch must refuse text-format results too")
	}
}

// The formats storm's connections actually use must keep passing, with the
// statement cache on and with it off, which is how a pool runs behind
// PgBouncer in transaction mode: caduceus asks for binary in both, so the
// mode that inverted every boolean under pgx has no hazard here.
func TestWireFormat_StormConnectionsPass(t *testing.T) {
	ctx := context.Background()
	for _, cache := range []int{0, -1} {
		pool, err := caduceusdrv.NewPoolConfig(ctx, caduceusdrv.PoolConfig{
			DSN: dsn(t), StatementCacheCapacity: cache,
		})
		if err != nil {
			t.Fatal(err)
		}
		rs, err := pool.Query(ctx, `
			SELECT false AS b, 42::int8 AS n, 'x'::text AS s, 'y'::varchar AS v,
			       '{"a":1}'::jsonb AS j, '\x0102'::bytea AS by,
			       gen_random_uuid() AS u, now() AS t, 1.5::numeric AS d,
			       '10.0.0.1/8'::inet AS i, tstzrange(now(), now() + interval '1 hour') AS r,
			       ARRAY[1.5, 2]::numeric[] AS ds`, nil)
		if err != nil {
			t.Fatalf("cache %d: a storm connection's result must be accepted: %v", cache, err)
		}
		if !rs.Next() {
			t.Fatalf("cache %d: no row: %v", cache, rs.Err())
		}
		raw := rs.RawValues()
		if runtime.Bool(raw[0]) {
			t.Fatal("binary false decoded as true")
		}
		if got := runtime.Int8(raw[1]); got != 42 {
			t.Fatalf("int8 = %d, want 42", got)
		}
		if got := string(raw[2]); got != "x" {
			t.Fatalf("text = %q", got)
		}
		if d, err := runtime.NumericErr(raw[8]); err != nil || d.String() != "1.5" {
			t.Fatalf("numeric = %v, %v; want 1.5", d, err)
		}
		if p, err := runtime.InetErr(raw[9]); err != nil || p.String() != "10.0.0.1/8" {
			t.Fatalf("inet = %v, %v", p, err)
		}
		if _, err := runtime.TstzRangeErr(raw[10]); err != nil {
			t.Fatalf("tstzrange: %v", err)
		}
		if ds, err := runtime.DecimalArray(raw[11]); err != nil || len(ds) != 2 {
			t.Fatalf("numeric[] = %v, %v", ds, err)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			t.Fatal(err)
		}
		pool.Close()
	}
}
