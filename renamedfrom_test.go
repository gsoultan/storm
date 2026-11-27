// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package storm_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/v2"
)

type rnAccount struct {
	storm.Model
	Handle string
}

func (a *rnAccount) Schema(t *storm.Table) {
	t.RenamedFrom("users")
	t.Col(&a.Handle).RenamedFrom("nick")
}

// RenamedFrom reaches the IR, where the migration differ reads it.
func TestRenamedFromReachesTheSchema(t *testing.T) {
	s, err := storm.Build(&rnAccount{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := s.Table("rn_accounts")
	if tbl == nil || tbl.RenamedFrom != "users" {
		t.Fatalf("table RenamedFrom lost: %+v", tbl)
	}
	if c := tbl.Column("handle"); c == nil || c.RenamedFrom != "nick" {
		t.Fatalf("column RenamedFrom lost: %+v", c)
	}
}

type rnClash struct {
	storm.Model
	Nick   string
	Handle string
}

func (a *rnClash) Schema(t *storm.Table) { t.Col(&a.Handle).RenamedFrom("nick") }

// A rename from a name the table still declares is two columns the plan could
// not tell apart.
func TestRenamedFromAColumnStillDeclaredIsRefused(t *testing.T) {
	_, err := storm.Build(&rnClash{})
	if err == nil || !strings.Contains(err.Error(), `RenamedFrom("nick")`) {
		t.Fatalf("want a refusal naming the rename, got %v", err)
	}
}
