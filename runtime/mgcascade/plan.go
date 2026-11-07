// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgcascade

// Plan is every collection a cascade can reach from Nodes[0], the collection
// whose package carries it. Edges name their child by index, so a reference
// that loops — a tree's parent_id — is a plan of finite size.
type Plan struct {
	Nodes []Node
}
