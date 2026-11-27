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

	// Options tunes every connection the pool opens.
	Options Options
}
