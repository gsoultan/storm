// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package liteddl

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/v2/schema"
)

// Check reports every portability problem in one pass — all of them, not the
// first, for the reason oraddl and msddl give: finding them one deploy at a
// time is the failure mode this package exists to replace.
func Check(s *schema.Schema) error {
	var problems []string
	enums := enumsOf(s)

	for _, t := range s.Tables {
		if len(t.Excludes) > 0 {
			problems = append(problems, fmt.Sprintf(
				"  %s%s: EXCLUDE constraints have no SQLite equivalent — the overlap they "+
					"prevent becomes a race the application cannot win", at(t.Pos), t.Name))
		}
		if t.Partition != nil || t.PartitionOf != "" {
			problems = append(problems, fmt.Sprintf(
				"  %s%s: SQLite has no table partitioning", at(t.Pos), t.Name))
		}
		if len(t.PrimaryKey) == 0 {
			problems = append(problems, fmt.Sprintf(
				"  %s%s: a table with no primary key has nothing for a foreign key to "+
					"reference and nothing for storm's write path to identify a row by",
				at(t.Pos), t.Name))
		}

		rowid := RowidKey(t)
		for _, c := range t.Columns {
			if c.Type.Enum {
				if _, ok := enums[c.Type.Name]; !ok {
					problems = append(problems, fmt.Sprintf(
						"  %s%s.%s: enum %s is not declared in the schema",
						at(colPos(t, c)), t.Name, c.Name, c.Type.Name))
				}
			} else if _, err := TypeSQL(t.Name, c); err != nil {
				problems = append(problems, "  "+at(colPos(t, c))+err.Error())
			}
			// A server-generated integer exists here only as THE rowid: one
			// per table, and the whole primary key.
			if (c.Identity || c.Serial) && c.Name != rowid {
				problems = append(problems, fmt.Sprintf(
					"  %s%s.%s: SQLite generates an integer only for a table's rowid, which "+
						"must be an integer column that is the whole primary key\n"+
						"      make it the sole primary key, or drop the identity and have "+
						"the application supply the value",
					at(colPos(t, c)), t.Name, c.Name))
			}
		}

		for _, fk := range t.ForeignKeys {
			if s.Table(fk.RefTable) == nil {
				// SQLite would accept it and refuse every write: a REFERENCES
				// clause is resolved when a row is written, not when the table
				// is made, so a missing parent is a runtime error, not a DDL one.
				problems = append(problems, fmt.Sprintf(
					"  %s%s: foreign key %s references %s, which is not in the model — "+
						"SQLite would create it and then refuse every insert",
					at(t.Pos), t.Name, fk.Name, fk.RefTable))
			}
		}
		for _, ix := range t.Indexes {
			checkIndex(t, ix, &problems)
		}
	}

	if len(s.Functions) > 0 || len(s.Views) > 0 || len(s.Triggers) > 0 {
		problems = append(problems, "  routines: storm does not generate SQLite views or "+
			"triggers, and SQLite has no stored functions — PL/pgSQL has no SQLite form")
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("this model does not port to SQLite:\n%s", strings.Join(problems, "\n"))
}

func checkIndex(t *schema.Table, ix *schema.Index, problems *[]string) {
	pos := t.Pos
	if len(ix.Columns) > 0 {
		if c := t.Column(ix.Columns[0].Name); c != nil && c.Pos != "" {
			pos = c.Pos
		}
	}
	name := IndexName(t, ix)
	if ix.Method != "" && ix.Method != "btree" {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s: index %s asks for method %s; every SQLite index is a B-tree",
			at(pos), t.Name, name, ix.Method))
	}
	if ix.Unique && len(ix.Include) > 0 {
		// Widening INCLUDE into trailing keys is exact for a lookup index and
		// wrong for a unique one, where the extra keys change what counts as
		// a duplicate: (email) INCLUDE (name) would admit two rows with one
		// email and different names.
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s: unique index %s has INCLUDE columns; SQLite has no covering-index "+
				"clause, and adding them as keys would weaken the uniqueness\n"+
				"      drop the INCLUDE, or declare a second, non-unique index that covers",
			at(pos), t.Name, name))
	}
	if ix.NullsNotDistinct {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s: index %s asks for NULLS NOT DISTINCT; SQLite's unique treats NULLs as "+
				"distinct and has no switch\n"+
				"      add a generated column that maps NULL to a sentinel and index that",
			at(pos), t.Name, name))
	}
	if len(ix.With) > 0 {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s: index %s carries storage parameters, which are PostgreSQL's spelling "+
				"and mean nothing here", at(pos), t.Name, name))
	}
	if ix.Invisible {
		*problems = append(*problems, fmt.Sprintf(
			"  %s%s: index %s is invisible; SQLite has no invisible indexes",
			at(pos), t.Name, name))
	}
	for _, c := range ix.Columns {
		if c.OpClass != "" || c.Prefix > 0 {
			*problems = append(*problems, fmt.Sprintf(
				"  %s%s: index %s uses an operator class or a prefix length; neither has "+
					"a SQLite form", at(pos), t.Name, name))
			break
		}
		if c.Collate != "" && !knownCollation(c.Collate) {
			*problems = append(*problems, fmt.Sprintf(
				"  %s%s: index %s uses collation %s; SQLite has BINARY, NOCASE and RTRIM "+
					"built in and storm does not register others", at(pos), t.Name, name, c.Collate))
			break
		}
	}
}

func knownCollation(name string) bool {
	switch strings.ToUpper(strings.Trim(name, `"`)) {
	case "C", "POSIX", "BINARY", "NOCASE", "RTRIM":
		return true
	}
	return false
}
