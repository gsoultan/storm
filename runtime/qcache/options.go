// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package qcache

import "time"

// Options configures a Cache. The zero value is usable and safe: it caches
// nothing that was not given a scope (see Scoped and Unscoped).
type Options struct {
	// Prefix starts every key the cache writes, so one Redis can serve
	// several applications or environments. Default "storm:qc:".
	Prefix string

	// MaxTTL caps the TTL any call may ask for, and is how long an
	// invalidation is remembered. Both have to be the same number: an
	// invalidation must outlive every entry written before it, or that entry
	// comes back when the invalidation expires. Default one hour.
	MaxTTL time.Duration

	// MaxBytes is the largest encoded result the cache will store. A result
	// that grows past it while being read is passed through and not stored,
	// and the copy made so far is dropped at once. Default 1 MiB.
	MaxBytes int

	// StoreTimeout bounds each round trip to the Store. A slow cache must not
	// make a query slower than having no cache at all, so on a timeout the
	// query goes to the database. Default 250ms.
	StoreTimeout time.Duration

	// Unscoped allows caching a call whose context carries no scope.
	//
	// Off by default because a cache that ignores who asked is a data leak:
	// the same SQL and arguments can return different rows to different
	// callers — row-level security, a session role, a search_path, a tenant
	// chosen by connection. Turn it on only when every cached query returns
	// the same rows to everyone, and say why where you do.
	Unscoped bool

	// OnError, if set, hears every error the cache swallowed: a Store that
	// failed or timed out, an entry that would not decode, a call refused for
	// having no scope. The query itself never fails because of the cache.
	OnError func(error)
}

func (o Options) withDefaults() Options {
	if o.Prefix == "" {
		o.Prefix = "storm:qc:"
	}
	if o.MaxTTL <= 0 {
		o.MaxTTL = time.Hour
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 1 << 20
	}
	if o.StoreTimeout <= 0 {
		o.StoreTimeout = 250 * time.Millisecond
	}
	return o
}
