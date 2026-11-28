// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package schemasqlite

// clause is one constraint read from a CREATE TABLE statement: its name, and
// what it is over. A CHECK has expr, and column when it was declared on one;
// a UNIQUE has cols; a FOREIGN KEY has cols and whether it is deferrable.
type clause struct {
	name, column, expr string
	cols               []string
	deferrable         bool
}
