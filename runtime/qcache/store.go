// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package qcache

import (
	"context"
	"time"
)

// Store is where cached results live. runtime/redisdrv's Client satisfies it
// against Redis (or anything that speaks RESP: Valkey, KeyDB, Dragonfly), and
// an adopter who already runs a client — go-redis, rueidis — satisfies it in
// a dozen lines without storm linking that client.
//
// Three methods, because a read-through cache needs exactly three things: read
// one entry, read the generations of its tags in one round trip, and write.
// There is no Delete: invalidation writes a new generation rather than
// deleting entries (see Cache.Invalidate), which is what makes it race-free.
type Store interface {
	// Get returns the value at key, or (nil, nil) when there is none. A miss
	// is not an error.
	Get(ctx context.Context, key string) ([]byte, error)

	// MGet returns the values at keys, in order, one round trip. A missing
	// key is a nil element, not an error.
	MGet(ctx context.Context, keys []string) ([][]byte, error)

	// Set writes value at key with an expiry. ttl is always positive.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}
