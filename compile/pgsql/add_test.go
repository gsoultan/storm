// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgsql_test

import (
	"testing"

	"github.com/gsoultan/storm/compile/pgsql"
)

// An add binds only the amount: the sum is the column's own value plus it, so
// two writers who add from one read both land.
func TestAddReadsTheColumnItWrites(t *testing.T) {
	if a, b := pgsql.AddFrag("hits"); a != `"hits" = "hits" + $` || b != "" {
		t.Errorf("AddFrag = %q, %q", a, b)
	}
}
