// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgcascade

// Node is one collection in a Plan and the references INTO it.
type Node struct {
	// Coll is the collection's name.
	Coll string
	// Edges are the references other collections hold to this one.
	Edges []Edge
}
