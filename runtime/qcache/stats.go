// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package qcache

// Stats counts what a Cache has done since it was made. Each field is read
// atomically on its own; the set is not a snapshot.
type Stats struct {
	// Hits were answered from the Store.
	Hits int64
	// Misses went to the database with a key, and are stored if read to the
	// end.
	Misses int64
	// Stores were written to the Store.
	Stores int64
	// Bypassed were marked cacheable but went to the database without a key:
	// an argument the cache cannot key on, no scope, or the Store failing to
	// report a tag's generation.
	Bypassed int64
	// Errors were reported to Options.OnError (or would have been).
	Errors int64
}
