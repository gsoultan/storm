// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgsplice

// Frag is one lowered comparison: A, then the value's hole, then B — or, when
// Arg is false, the whole predicate in A. Generated code builds a table of
// them from compile/mongo at generate time.
type Frag struct {
	A, B string
	Arg  bool
}
