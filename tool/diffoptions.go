// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package tool

// diffOptions are the flags `storm diff` reads.
type diffOptions struct {
	allowDestructive bool
	concurrently     bool
	replay           bool
	down             bool
	dryRun           bool
}
