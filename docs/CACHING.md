---
tags: [storm, caching, redis]
updated: 2026-11-02
status: experimental — see docs/STABILITY.md
---

# Caching query results in Redis

storm can answer a read from Redis instead of the database: `runtime/qcache`
is a read-through cache that decorates the `Executor` port, and
`runtime/redisdrv` is a small stdlib-only Redis client that serves as its
store. Generated packages do not change — they are handed a different
`Executor`.

Redis is a **cache** here, not a target. A key-value store has no joins, no
predicates over columns and no transactions across keys, so as a place to
generate *for* it would refuse most of what a model declares; as a place to
keep answers the database already gave, it is the usual tool.

## Wiring

```go
import (
    "github.com/gsoultan/storm/runtime/qcache"
    "github.com/gsoultan/storm/runtime/redisdrv"
)

rc, err := redisdrv.OpenURL("rediss://:password@cache.internal:6380/0")
if err != nil { ... }
if err := rc.Ping(ctx); err != nil { ... }       // Open does not dial

cached := qcache.New(pool, rc, qcache.Options{
    OnError: func(err error) { log.Warn("cache", "err", err) },
})
```

`cached` is a `runtime.Executor`. Pass it wherever a generated read takes one.
Keep the pool itself for transactions (see below).

## Marking a read

Nothing is cached unless the call's context says so, at the call site, with a
TTL and the tags a write will invalidate:

```go
// once per request, where identity is known
ctx = qcache.Scoped(ctx, tenantID)

// at the read
list, err := product.New().
    Where(product.Active.Eq(true)).
    List(qcache.For(ctx, time.Minute, "products"), cached)

// after the write that changes it has COMMITTED
if err := cached.Invalidate(ctx, "products"); err != nil { ... }
```

An unmarked read through `cached` goes to the database, costing one context
lookup and no allocation (asserted by `TestAnUnmarkedQueryAllocatesNothing`).

## What it promises

| | |
|---|---|
| **Key** | SHA-256 of the scope, the SQL text, every bound argument (typed: `int32(1)` and `int64(1)` are two keys) and the current generation of every tag |
| **Scope** | Required. A marked call with no `qcache.Scoped` runs uncached and reports `qcache.ErrNoScope`, unless `Options.Unscoped` says every cached query returns the same rows to everyone. Row-level security, a session role or a tenant per connection all make the same SQL return different rows |
| **What is stored** | A result read to the end without error. A read that stops early (`First`, `Take`) saw a prefix; storing it would answer the next call wrongly. A result larger than `Options.MaxBytes` (1 MiB) is passed through, and its copy dropped the moment it outgrows the limit |
| **Shape** | Byte rows replay as byte rows, value rows (database/sql adapters, Oracle) as `runtime.ValueRows`. NULL and empty stay distinct |
| **Invalidation** | `Invalidate(tags...)` writes each tag a fresh random generation; every entry keyed on the old one becomes unreachable, for every scope. Nothing is deleted, so there is no index of entries to maintain and no race with a reader storing a result it read before the write |
| **TTL** | Per call, capped at `Options.MaxTTL` (1 h). A generation lives `MaxTTL` + 1 min, so it outlives every entry written before it |
| **Failure** | A Store that is down, slow (`Options.StoreTimeout`, 250 ms) or returns a corrupt entry costs a miss and an `OnError` call. A cached read never fails because of the cache. `Invalidate` does return its error: a lost invalidation means stale answers |
| **Not cached** | `Exec`, `CopyFrom`, `Batch` — always passed through |

## What it does not do

- **Guess what a write touched.** The cache sees SQL text, not tables;
  parsing it to infer invalidation is how caches end up wrong. A write names
  its tags.
- **Cache inside a transaction.** A read in a transaction must see that
  transaction's writes. Wrap the pool; `pool.StartTx` returns an uncached
  `Tx`, which is the correct thing to read through inside it.
- **Invalidate before commit.** Called before, a concurrent reader can load
  the old rows from the database and store them under the new generation.
- **Survive eviction of a generation key.** Under `allkeys-*` eviction Redis
  may evict a tag's generation before its entries, which brings entries from
  before the last invalidation back. Use `volatile-*` or `noeviction`, or a
  Redis database the cache has to itself with enough memory.
- **Collapse a stampede.** N concurrent misses on one key run N queries.

## The Redis client

`runtime/redisdrv` speaks RESP2 to Redis, Valkey, KeyDB, Dragonfly and their
managed versions: TCP or TLS (`rediss://`), `AUTH` with or without a user,
`SELECT`, a pool bounded by `MaxOpen` with `MaxIdle` kept warm, and a deadline
on every round trip from the context or `IOTimeout`. A malformed reply
discards its connection. Replies are bounded by `MaxReplyBytes` and nesting
depth, so a hostile server cannot make it allocate without limit.

It is stdlib-only for the reason `runtime/mydrv` is: nothing under `runtime/`
links a third-party module. If you already run go-redis or rueidis, implement
`qcache.Store` — `Get`, `MGet`, `Set` with a TTL — with it in a dozen lines and
skip `redisdrv` entirely. Cluster and Sentinel are not handled; point it at a
primary or a proxy.

Live tests run with `STORM_REDIS_URL=redis://localhost:6379/15`. They
**flush** the database the URL names.
