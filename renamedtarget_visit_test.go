// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package storm_test

import "github.com/gsoultan/storm"

// rnVisit belongs to a person whose table is renamed; see
// TestForeignKeyFollowsTargetRename.
type rnVisit struct {
	storm.Model
	Person rnPerson
}
