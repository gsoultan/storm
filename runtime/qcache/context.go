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

type ctxKey int

const (
	policyKey ctxKey = iota
	scopeKey
)

// policy is what For attached to a context.
type policy struct {
	ttl  time.Duration
	tags []string
}

// For marks the queries run with ctx as cacheable for ttl, under tags.
//
// Nothing is cached without it. Caching is a decision about one read at one
// call site — how stale it may be and what invalidates it — so it is made
// there, not configured globally:
//
//	ctx := qcache.For(ctx, 30*time.Second, "products")
//	list, err := product.Where(...).List(ctx, db)
//
// A tag names what a write must invalidate: Cache.Invalidate(ctx, "products")
// after a write to the products table makes every entry read under that tag
// unreachable. A call with no tags expires by TTL alone.
//
// A ttl of zero or less disables caching for ctx, which is how a caller turns
// it off for a context it was handed.
func For(ctx context.Context, ttl time.Duration, tags ...string) context.Context {
	return context.WithValue(ctx, policyKey, &policy{ttl: ttl, tags: tags})
}

// Scoped records who is asking, and becomes part of every cache key derived
// from ctx: two scopes never share an entry.
//
// Set it where identity is known — request middleware, after authentication —
// to whatever decides which rows a query can see: a tenant, a user, a
// database role. Setting it again replaces it.
func Scoped(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, scopeKey, scope)
}

func policyFrom(ctx context.Context) (*policy, bool) {
	p, ok := ctx.Value(policyKey).(*policy)
	return p, ok && p.ttl > 0
}

func scopeFrom(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(scopeKey).(string)
	return s, ok
}
