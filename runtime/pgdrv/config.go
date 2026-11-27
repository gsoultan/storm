// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgdrv

import "time"

// Config describes a Pool.
type Config struct {
	// DSN is a URL (postgres://user:pass@host:5432/db?sslmode=require) or
	// key=value pairs, with the PG* environment variables and the password
	// file filling in what it leaves out, as for libpq.
	DSN string

	// MaxConns caps the connections the pool opens. Zero is DefaultMaxConns.
	MaxConns int

	// AcquireTimeout bounds a wait for a free connection. Zero is
	// DefaultAcquireTimeout; negative waits as long as the context allows.
	AcquireTimeout time.Duration

	// Database, when set, is the database to connect to in place of the
	// DSN's: one DSN for a cluster, one pool per shard or tenant database.
	Database string

	// RuntimeParams are set on every connection at startup, over what the
	// DSN says: search_path, application_name, statement_timeout and so on.
	RuntimeParams map[string]string

	// Options tunes every connection the pool opens.
	Options Options
}
