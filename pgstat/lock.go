// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgstat

// Lock is a lock a session holds or waits for on a relation.
type Lock struct {
	// Type is pg_locks.locktype: relation, tuple, and so on.
	Type    string
	Schema  string
	Table   string
	Granted bool
}
