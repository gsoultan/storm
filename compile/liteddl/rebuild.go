// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package liteddl

import (
	"strings"

	"github.com/gsoultan/storm/v2/schema"
)

// RebuildPrefix starts the name a table is rebuilt under before it takes the
// old one's. A table whose name starts with it is storm's, mid-migration.
const RebuildPrefix = "_storm_rebuild_"

// Rebuild is the statements that take a table from old to cur by building it
// again — SQLite's answer to every ALTER it does not have.
//
// SQLite's ALTER TABLE renames a table, renames a column, adds a column with
// restrictions, and drops one with more. A column's type, its nullability, its
// default; a table's key, its uniques, its checks, its foreign keys: none of
// those has an ALTER. What the SQLite documentation prescribes instead
// (https://sqlite.org/lang_altertable.html#otheralter) is this:
//
//  1. create the table as it should be, under another name;
//  2. copy every row across;
//  3. drop the old table;
//  4. rename the new one to the old name;
//  5. create its indexes again, since they went with the old table.
//
// Foreign key ENFORCEMENT must be off while it runs, and that is the caller's
// to arrange, outside the transaction: with it on, step 3 is an implicit
// DELETE of every row, which cascades into every child table — and PRAGMA
// foreign_keys is a no-op inside a transaction. migrate.AutoSQLite turns it
// off, runs the migration in one transaction, and checks every key with
// PRAGMA foreign_key_check before it commits.
//
// The rows are copied by column NAME: a column in both keeps its values, one
// only in cur gets its default, and one only in old is gone. old is the table
// as it stands once every rename has run, so a renamed column is copied under
// its new name. A generated column is never written — SQLite computes it — but
// a column that STOPS being generated is copied from what the old one computed,
// which is the value PostgreSQL's DROP EXPRESSION keeps.
func Rebuild(old, cur *schema.Table, enums map[string]*schema.Enum) ([]string, error) {
	tmp := RebuildPrefix + cur.Name
	create, err := createTableNamed(Ident(tmp), cur, enums)
	if err != nil {
		return nil, err
	}
	out := []string{create}
	var cols []string
	for _, c := range cur.Columns {
		if c.Generated != "" || old.Column(c.Name) == nil {
			continue
		}
		cols = append(cols, Ident(c.Name))
	}
	if len(cols) > 0 {
		list := strings.Join(cols, ", ")
		out = append(out, "INSERT INTO "+Ident(tmp)+" ("+list+") SELECT "+list+" FROM "+Ident(cur.Name))
	}
	out = append(out,
		"DROP TABLE "+Ident(cur.Name),
		"ALTER TABLE "+Ident(tmp)+" RENAME TO "+Ident(cur.Name))
	return append(out, Indexes(cur)...), nil
}

// AddsInPlace reports whether ALTER TABLE ... ADD COLUMN can add c to a table
// that already has rows. SQLite refuses a key column, a stored generated
// column, an expression default, and a NOT NULL column without a non-NULL
// default; every other change to a column is a Rebuild.
func AddsInPlace(t *schema.Table, c *schema.Column) bool {
	if c.Generated != "" || c.Identity || c.Serial {
		return false
	}
	for _, k := range t.PrimaryKey {
		if k == c.Name {
			return false
		}
	}
	d := Default(c)
	if d != "" && !isConstant(d) {
		return false
	}
	switch strings.ToUpper(d) {
	case "CURRENT_TIMESTAMP", "CURRENT_DATE", "CURRENT_TIME":
		return false // constant enough for CREATE TABLE, not for ADD COLUMN
	}
	if c.NotNull && (d == "" || strings.EqualFold(d, "NULL")) {
		return false
	}
	return true
}
