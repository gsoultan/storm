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
	"strings"
	"sync"
	"testing"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgdrv"
)

// The tracing claim, proved rather than asserted — and the proof corrected
// the claim.
//
// Every round trip storm makes goes through the Executor and therefore
// through caduceus, so caduceus's tracers see all of it. But tracing is split
// across SEPARATE interfaces, and a QueryTracer alone is blind to batches: this
// test failed on exactly that under pgx, which matters because a batch is where a plan's
// relation loads live. An adopter who wired only QueryTracer would watch the
// one query they wrote and never see the four the plan issued, then conclude
// the ORM hides work.
//
// So the recipe — in docs/DEPLOYMENT.md, and executable here — is one type
// implementing all three: QueryTracer for Query/Exec, BatchTracer for Batch,
// CopyFromTracer for bulk loads. storm has no tracing API of its own on
// purpose: an interface it invented would be one more thing to learn and one
// more thing to keep faithful.
type recordingTracer struct {
	mu   sync.Mutex
	sqls []string
}

func (r *recordingTracer) TraceQueryStart(ctx context.Context, _ *caduceus.Conn, data caduceus.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sqls = append(r.sqls, data.SQL)
	return ctx
}

func (r *recordingTracer) TraceQueryEnd(context.Context, *caduceus.Conn, caduceus.TraceQueryEndData) {
}

// Batch: caduceus reports the batch with every statement in it.
func (r *recordingTracer) TraceBatchStart(ctx context.Context, _ *caduceus.Conn, data caduceus.TraceBatchStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range data.Queries {
		r.sqls = append(r.sqls, q.SQL)
	}
	return ctx
}

func (r *recordingTracer) TraceBatchEnd(context.Context, *caduceus.Conn, caduceus.TraceBatchEndData) {
}

// CopyFrom carries no SQL, so record it as the operation it is.
func (r *recordingTracer) TraceCopyFromStart(ctx context.Context, _ *caduceus.Conn, data caduceus.TraceCopyFromStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sqls = append(r.sqls, "COPY "+data.TableName.Sanitize())
	return ctx
}

func (r *recordingTracer) TraceCopyFromEnd(context.Context, *caduceus.Conn, caduceus.TraceCopyFromEndData) {
}

func (r *recordingTracer) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sqls...)
}

func TestTracer_SeesEveryStatementStormIssues(t *testing.T) {
	ctx := context.Background()
	tr := &recordingTracer{}
	pool, err := pgdrv.NewPoolConfig(ctx, pgdrv.Config{DSN: dsn(t), Options: pgdrv.Options{Tracer: tr}})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ex := pool

	rows, err := ex.Query(ctx, `SELECT 1::int8 AS one`, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()

	if _, err := ex.Exec(ctx, `SELECT set_config('storm.probe', $1, true)`, []any{"x"}); err != nil {
		t.Fatal(err)
	}

	// A batch is one round trip carrying several statements; the tracer must
	// see each, or "no hidden queries" would be unverifiable from outside.
	err = ex.Batch(ctx, []runtime.BatchOp{
		{SQL: `SELECT 2::int8 AS two`, WantRows: true},
		{SQL: `SELECT 3::int8 AS three`, WantRows: true},
	}, func(_ int, r runtime.Rows, _ int64, err error) error {
		if err != nil {
			return err
		}
		for r.Next() {
		}
		return r.Err()
	})
	if err != nil {
		t.Fatal(err)
	}

	seen := strings.Join(tr.seen(), "\n")
	for _, want := range []string{"SELECT 1::int8", "set_config", "SELECT 2::int8", "SELECT 3::int8"} {
		if !strings.Contains(seen, want) {
			t.Errorf("the tracer never saw %q; it saw:\n%s", want, seen)
		}
	}
}

// CountingExecutor is the other half of the same promise: round-trip counts
// are assertable in an adopter's own tests, not just in storm's.
func TestCountingExecutor_CountsWhatStormSends(t *testing.T) {
	ctx := context.Background()
	pool, err := pgdrv.NewPool(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	counting := &runtime.CountingExecutor{Inner: pool}
	for i := 0; i < 3; i++ {
		rows, err := counting.Query(ctx, `SELECT 1::int8`, nil)
		if err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	if got := counting.RoundTrips(); got != 3 {
		t.Fatalf("counted %d round trips, sent 3", got)
	}
}
