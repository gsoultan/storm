// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package migratesum

import (
	"fmt"
	"strings"
)

// Error lists every way a directory disagrees with its storm.sum.
type Error struct {
	Modified   []string // in the sum, with different content now
	Added      []string // on disk, not in the sum
	Removed    []string // in the sum, gone from disk
	Duplicates []string // two up migrations claiming one version
	Corrupt    bool     // the total does not match the entries the file lists
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("the migrations directory does not match " + File + ":\n")
	list := func(what string, names []string) {
		for _, n := range names {
			fmt.Fprintf(&b, "  %-9s %s\n", what, n)
		}
	}
	list("modified", e.Modified)
	list("added", e.Added)
	list("removed", e.Removed)
	list("version", e.Duplicates)
	if e.Corrupt {
		b.WriteString("  " + File + " itself was edited: its first line is not the hash of the rest\n")
	}
	switch {
	case len(e.Duplicates) > 0:
		b.WriteString("two migrations share a version, which is what two branches that each ran\n" +
			"`storm diff` leave behind; regenerate the newer one on top of the other\n")
	case len(e.Modified) > 0 || len(e.Removed) > 0:
		b.WriteString("a migration that has run anywhere is history: add a new migration instead of\n" +
			"editing one. If the edit is deliberate and nothing has applied the file yet,\n" +
			"run `storm sum` to record it\n")
	default:
		b.WriteString("migrations written by hand are fine; run `storm sum` to record them\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
