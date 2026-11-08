// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgcascade

// Edge is one reference: a child collection's fields that hold a parent's
// values, and the action its declaration names.
type Edge struct {
	// Child is the referencing collection's index in the Plan.
	Child int
	// Name is the reference as the model spells it, for an error:
	// "books.author_id".
	Name string
	// From are the child's stored fields; To the parent's, pairwise.
	From, To []string
	Action   Action
	// Set is the pipeline stage SetNull and SetDefault apply to a child:
	// [{"$set":{...}}], written at generate time.
	Set string
}
