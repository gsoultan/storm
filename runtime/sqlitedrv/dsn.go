// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlitedrv

import "strings"

// DriverName is the database/sql name modernc.org/sqlite registers.
const DriverName = "sqlite"

// DSN is a modernc.org/sqlite connection string for path with the settings
// storm's lowering relies on. Each one changes an ANSWER, not a speed:
//
//   - foreign_keys(1). SQLite does not enforce a foreign key unless the
//     connection asks, per connection, every time. Without it ON DELETE
//     CASCADE does nothing and a dangling reference is accepted.
//   - case_sensitive_like(1). SQLite's LIKE folds ASCII case by default,
//     which would make storm's Like mean ILike.
//   - _txlock=immediate. A transaction takes the write lock at BEGIN rather
//     than at its first write. This is what storm's row-lock modes rest on
//     (see compile/sqlite), and it turns the deadlock two deferred
//     transactions reach on upgrading — SQLITE_BUSY, immediately, whatever
//     the timeout — into a wait.
//   - busy_timeout(5000). A writer waits five seconds for another to finish
//     rather than failing at once with SQLITE_BUSY.
//   - journal_mode(WAL), for a file. Readers no longer block the writer, nor
//     the writer readers. An in-memory database has no journal to choose.
//
// The path may be ":memory:" or a "file:" URI; anything already in its query
// string is kept, and wins.
func DSN(path string) string {
	params := []string{
		"_pragma=foreign_keys(1)",
		"_pragma=case_sensitive_like(1)",
		"_pragma=busy_timeout(5000)",
		"_txlock=immediate",
	}
	if !strings.Contains(path, ":memory:") && !strings.Contains(path, "mode=memory") {
		params = append(params, "_pragma=journal_mode(WAL)")
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + strings.Join(params, "&")
}
