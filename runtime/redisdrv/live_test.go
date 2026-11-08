// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package redisdrv_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/qcache"
	"github.com/gsoultan/storm/runtime/redisdrv"
)

var _ qcache.Store = (*redisdrv.Client)(nil)

// live returns a client for STORM_REDIS_URL (e.g. redis://localhost:6379/15),
// or skips. The database it names is FLUSHED: point it at a scratch one.
func live(t *testing.T) *redisdrv.Client {
	t.Helper()
	url := os.Getenv("STORM_REDIS_URL")
	if url == "" {
		t.Skip("STORM_REDIS_URL not set")
	}
	c, err := redisdrv.OpenURL(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Do(context.Background(), "FLUSHDB"); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLiveCommands(t *testing.T) {
	c, ctx := live(t), context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if v, err := c.Get(ctx, "missing"); v != nil || err != nil {
		t.Fatalf("missing key: %q, %v", v, err)
	}
	binary := []byte("a\r\nb\x00c")
	if err := c.Set(ctx, "k", binary, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(ctx, "empty", []byte{}, 0); err != nil {
		t.Fatal(err)
	}
	if v, err := c.Get(ctx, "k"); err != nil || !bytes.Equal(v, binary) {
		t.Fatalf("got %q, %v", v, err)
	}
	if v, err := c.Get(ctx, "empty"); err != nil || v == nil || len(v) != 0 {
		t.Fatalf("an empty value must read as empty, not missing: %#v, %v", v, err)
	}
	vs, err := c.MGet(ctx, []string{"k", "nope", "empty"})
	if err != nil || len(vs) != 3 || !bytes.Equal(vs[0], binary) || vs[1] != nil || vs[2] == nil {
		t.Fatalf("MGet %q, %v", vs, err)
	}
	if ttl, err := c.Do(ctx, "PTTL", "k"); err != nil || ttl.(int64) <= 0 || ttl.(int64) > 60000 {
		t.Fatalf("PTTL %v, %v", ttl, err)
	}
	if n, err := c.Del(ctx, "k", "nope"); err != nil || n != 1 {
		t.Fatalf("Del %d, %v", n, err)
	}
	var re *redisdrv.Error
	if _, err := c.Do(ctx, "NOSUCHCOMMAND"); !errors.As(err, &re) {
		t.Fatalf("an error reply: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal("the connection did not survive an error reply:", err)
	}
	if _, err := c.Do(ctx, "INCR", "empty"); err == nil {
		t.Fatal("INCR of a non-integer succeeded")
	}
}

func TestLiveConcurrent(t *testing.T) {
	c, ctx := live(t), context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := "c" + string(rune('a'+i%26))
			if err := c.Set(ctx, key, []byte(key), time.Minute); err != nil {
				errs <- err
				return
			}
			if v, err := c.Get(ctx, key); err != nil || string(v) != key {
				errs <- errors.New("read back " + string(v))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// The whole point: a generated read through qcache, backed by a real Redis.
func TestLiveAsQueryCache(t *testing.T) {
	rc := live(t)
	ex := &countExec{}
	c := qcache.New(ex, rc, qcache.Options{})
	ctx := qcache.For(qcache.Scoped(context.Background(), "t1"), time.Minute, "items")
	read := func() {
		rows, err := c.Query(ctx, "select", []any{int64(1)})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			if string(rows.RawValues()[0]) != "v" || rows.RawValues()[1] != nil {
				t.Fatal("wrong row")
			}
			n++
		}
		rows.Close()
		if n != 2 {
			t.Fatalf("%d rows", n)
		}
	}
	read()
	read()
	if ex.n != 1 {
		t.Fatalf("database saw %d queries, want 1", ex.n)
	}
	if err := c.Invalidate(context.Background(), "items"); err != nil {
		t.Fatal(err)
	}
	read()
	if ex.n != 2 {
		t.Fatalf("after invalidation the database saw %d queries, want 2", ex.n)
	}
	if s := c.Stats(); s.Errors != 0 {
		t.Fatalf("stats %+v", s)
	}
}

type countExec struct{ n int }

func (e *countExec) Query(context.Context, string, []any) (runtime.Rows, error) {
	e.n++
	return &twoRows{}, nil
}
func (e *countExec) Exec(context.Context, string, []any) (int64, error) { return 0, nil }
func (e *countExec) CopyFrom(context.Context, string, []string, runtime.CopySource) (int64, error) {
	return 0, nil
}
func (e *countExec) Batch(context.Context, []runtime.BatchOp, func(int, runtime.Rows, int64, error) error) error {
	return nil
}

type twoRows struct{ at int }

func (r *twoRows) Next() bool          { r.at++; return r.at <= 2 }
func (r *twoRows) RawValues() [][]byte { return [][]byte{[]byte("v"), nil} }
func (r *twoRows) Close()              {}
func (r *twoRows) Err() error          { return nil }
