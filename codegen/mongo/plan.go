// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package codegenmongo

import "github.com/gsoultan/storm/schema"

// plan is a row type and the query that fills it: one per relation, and one
// per plan the model named.
type plan struct {
	name      string
	parent    *schema.Table
	parentPkg string
	members   []member
}
