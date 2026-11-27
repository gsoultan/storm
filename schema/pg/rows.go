// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package schemapg

// Rows is a result introspection reads, row by row. pgx.Rows is one, and so
// is any driver's result that scans into Go values.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}
