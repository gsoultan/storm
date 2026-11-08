// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package qcache is an opt-in, read-through cache of query results in front
// of any runtime.Executor, with Redis (runtime/redisdrv) as its usual Store.
//
// It is a decorator on the port, like runtime.CountingExecutor: wrap the
// Executor once, hand the wrapper to generated code, and nothing about the
// generated package changes.
//
//	rc, _ := redisdrv.Open(redisdrv.Options{Addr: "localhost:6379"})
//	db := qcache.New(pool, rc, qcache.Options{})
//
//	ctx = qcache.Scoped(ctx, tenantID)                 // in middleware
//	ctx := qcache.For(ctx, time.Minute, "products")    // at the call site
//	list, err := product.Where(...).List(ctx, db)      // cached
//	err = product.Insert(ctx, db, &p)                  // never cached
//	err = db.Invalidate(ctx, "products")               // after the commit
//
// What it will not do, each on purpose:
//
//   - Cache anything it was not asked to. A query whose context did not pass
//     through For goes straight to the database, at the cost of one context
//     lookup. Staleness is a decision about one read, made where it is read.
//   - Cache across callers by default. The scope from Scoped is part of every
//     key, and a call with no scope is not cached unless Options.Unscoped
//     says every cached query returns the same rows to everyone.
//   - Invalidate implicitly. The cache sees SQL text, not tables, and parsing
//     SQL to guess what a write touched is how a cache ends up wrong. Writes
//     name their tags, in Invalidate.
//   - Fail a query. A Store that is down, slow or returns garbage costs a
//     cache miss and a call to Options.OnError, never an error to the caller.
//   - Cache Exec, CopyFrom or Batch, which pass straight through.
//
// Wrap a pool, not a transaction. A read inside a transaction must see the
// transaction's own writes, which a cache shared with other connections
// cannot know about; runtime.DB.StartTx on the pool you wrapped hands back an
// uncached Tx, which is the right thing.
package qcache

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// ErrNoScope is reported to Options.OnError when a cacheable call has no
// scope and Options.Unscoped is off. The call runs uncached.
var ErrNoScope = errors.New("qcache: call marked cacheable has no scope " +
	"(qcache.Scoped); it ran uncached — set Options.Unscoped only if every " +
	"cached query returns the same rows to every caller")

// Cache is a runtime.Executor that answers marked reads from a Store.
type Cache struct {
	inner runtime.Executor
	store Store
	opt   Options

	hits, misses, stores, bypassed, errs atomic.Int64
}

// New wraps inner. The Cache is safe for concurrent use if inner and store
// are.
func New(inner runtime.Executor, store Store, opt Options) *Cache {
	return &Cache{inner: inner, store: store, opt: opt.withDefaults()}
}

// Query answers from the Store when ctx was marked with For and an entry for
// this scope, SQL, arguments and tag generations exists; otherwise it runs the
// query and, when the caller reads it to the end, stores the result.
func (c *Cache) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	p, ok := policyFrom(ctx)
	if !ok {
		return c.inner.Query(ctx, sql, args)
	}
	key, ok := c.key(ctx, p, sql, args)
	if !ok {
		c.bypassed.Add(1)
		return c.inner.Query(ctx, sql, args)
	}

	sctx, cancel := context.WithTimeout(ctx, c.opt.StoreTimeout)
	entry, err := c.store.Get(sctx, key)
	cancel()
	switch {
	case err != nil:
		c.fail(fmt.Errorf("qcache: get: %w", err))
	case entry != nil:
		if h, err := decode(entry); err == nil {
			c.hits.Add(1)
			return h.rows(), nil
		}
		c.fail(errCorrupt)
	}

	c.misses.Add(1)
	rows, err := c.inner.Query(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	ttl := min(p.ttl, c.opt.MaxTTL)
	if vr, ok := rows.(runtime.ValueRows); ok {
		return (*valueRecorder)(&recorder{inner: rows, vals: vr, c: c, ctx: ctx, key: key, ttl: ttl,
			buf: header(shapeValues)}), nil
	}
	return &recorder{inner: rows, c: c, ctx: ctx, key: key, ttl: ttl, buf: header(shapeBytes)}, nil
}

// key derives the entry key, or reports why this call cannot have one.
func (c *Cache) key(ctx context.Context, p *policy, sql string, args []any) (string, bool) {
	scope, scoped := scopeFrom(ctx)
	if !scoped && !c.opt.Unscoped {
		c.fail(ErrNoScope)
		return "", false
	}
	var gens [][]byte
	if len(p.tags) > 0 {
		keys := make([]string, len(p.tags))
		for i, t := range p.tags {
			keys[i] = tagKey(c.opt.Prefix, t)
		}
		sctx, cancel := context.WithTimeout(ctx, c.opt.StoreTimeout)
		g, err := c.store.MGet(sctx, keys)
		cancel()
		if err == nil && len(g) != len(keys) {
			err = fmt.Errorf("MGet returned %d values for %d keys", len(g), len(keys))
		}
		if err != nil {
			// Without the generations there is no telling whether an entry
			// predates an invalidation: no read, and no write either.
			c.fail(fmt.Errorf("qcache: tag generations: %w", err))
			return "", false
		}
		gens = g
	}
	return entryKey(c.opt.Prefix, scope, sql, args, p.tags, gens)
}

// put stores a recorded result. The query has already succeeded, so the
// caller's cancellation does not stop the write — only StoreTimeout does.
func (c *Cache) put(ctx context.Context, key string, entry []byte, ttl time.Duration) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.opt.StoreTimeout)
	defer cancel()
	if err := c.store.Set(sctx, key, entry, ttl); err != nil {
		c.fail(fmt.Errorf("qcache: set: %w", err))
		return
	}
	c.stores.Add(1)
}

// Invalidate makes every entry read under any of tags unreachable, for every
// scope. Call it after the write it answers for has COMMITTED: before, a
// concurrent reader can still read the old rows from the database and store
// them under the new generation.
//
// It writes each tag a fresh random generation rather than deleting entries,
// which needs no index of entries and has no race with a reader storing an old
// result: a reader that fetched the generation before the invalidation stores
// under a key nobody will compute again. The generation lives for MaxTTL plus
// a minute, longer than any entry written before it.
//
// Unlike a read, an invalidation that fails returns its error. A read that
// misses the cache is slower; a write whose invalidation was lost leaves
// stale answers for up to the TTL, and the caller has to know.
func (c *Cache) Invalidate(ctx context.Context, tags ...string) error {
	var errs []error
	for _, t := range tags {
		var gen [16]byte
		_, _ = rand.Read(gen[:])
		if err := c.store.Set(ctx, tagKey(c.opt.Prefix, t), gen[:], c.opt.MaxTTL+time.Minute); err != nil {
			errs = append(errs, fmt.Errorf("qcache: invalidate %q: %w", t, err))
		}
	}
	return errors.Join(errs...)
}

// Exec is never cached.
func (c *Cache) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	return c.inner.Exec(ctx, sql, args)
}

// CopyFrom is never cached.
func (c *Cache) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	return c.inner.CopyFrom(ctx, table, cols, src)
}

// Batch is never cached: its statements are writes, or reads whose results
// are streamed into a callback the cache cannot replay.
func (c *Cache) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	return c.inner.Batch(ctx, ops, each)
}

// Stats reports what the cache has done so far.
func (c *Cache) Stats() Stats {
	return Stats{
		Hits:     c.hits.Load(),
		Misses:   c.misses.Load(),
		Stores:   c.stores.Load(),
		Bypassed: c.bypassed.Load(),
		Errors:   c.errs.Load(),
	}
}

func (c *Cache) fail(err error) {
	c.errs.Add(1)
	if c.opt.OnError != nil {
		c.opt.OnError(err)
	}
}

var _ runtime.Executor = (*Cache)(nil)
