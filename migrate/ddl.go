// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package migrate

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/v2/compile/pgddl"
	"github.com/gsoultan/storm/v2/schema"
)

// Dialect selects the DDL a plan is rendered in.
//
// It is not a runtime branch in the sense the rest of storm forbids one: a
// migration tool is not the generated code, and the dialect is decided once,
// at the top of a diff, from a flag. What the rule rules out — a query
// deciding its own syntax per call — is not what happens here.
type Dialect string

const (
	Postgres Dialect = "postgres"
	MSSQL    Dialect = "mssql"
	Oracle   Dialect = "oracle"
)

// ddl is the seam every statement in this package is written through.
//
// The shape is codegen.lowering's: a struct of function values, filled in once
// per dialect, rather than an interface or a switch at each call site. Same
// reasoning, too. The dialect is known before the first statement is rendered,
// so it should be decided ONCE; and a back end that has no such statement can
// say so by leaving the field nil, which an interface cannot express without
// inventing a second method to ask first.
//
// WHAT IS DELIBERATELY NOT BEHIND THE SEAM:
//
//   - Comparison. narrowing, canonical, sameKeys and sameParams decide whether
//     two schemas DIFFER. That is a question about the model, not about SQL,
//     and every schema reaching this code has already been through its own
//     dialect's Check.
//
//   - Routines. Functions, views and triggers are diffed by routinediff.go,
//     which is PostgreSQL-only because no other target has a routine model in
//     schema yet. They are absent from this struct rather than nil in it: a
//     seam field that nothing ever fills in is a promise the package has not
//     made.
//
//   - Introspection and normalisation. Reading a live database back is the
//     other half of the problem and is a driver's job, not a renderer's. See
//     normalize.go and normalize_mssql.go, which are separate entry points for
//     the same reason: they take different connections.
type ddl struct {
	// Ident quotes a name.
	Ident func(string) string

	// Enums reconciles two schemas' enum declarations.
	//
	// This is a whole STEP rather than the three statements it is in
	// PostgreSQL, because the back ends disagree about what an enum IS.
	// PostgreSQL has a TYPE: an object created, extended and dropped
	// independently of the columns using it. SQL Server has no enum at all —
	// msddl renders the labels as a CHECK constraint per COLUMN — so there is
	// no statement there to put behind a per-statement seam.
	Enums func(p *Plan, from, to *schema.Schema) error

	// DropEnums is the tail of the same step, kept apart because the ORDER is
	// the point: a type is only droppable once the last table using it is
	// gone, and those drops are emitted between the two. Nil where Enums is a
	// refusal rather than a renderer.
	DropEnums func(p *Plan, from, to *schema.Schema)

	CreateTable   func(*schema.Table) (string, error)
	DropTable     func(*schema.Table) string
	AddForeignKey func(*schema.Table, *schema.ForeignKey) string

	AddColumn  func(*schema.Table, *schema.Column) (string, error)
	DropColumn func(*schema.Table, *schema.Column) string

	// AlterColumnType, SetNotNull and DropNotNull all take the whole column
	// rather than the one fact that changed, because SQL Server's ALTER COLUMN
	// restates the type AND the nullability on every change — a renderer told
	// only "this column is now NOT NULL" could not write the statement, and
	// one that guessed the type would quietly retype the column.
	AlterColumnType func(*schema.Table, *schema.Column) (string, error)
	SetNotNull      func(*schema.Table, *schema.Column) (string, error)
	DropNotNull     func(*schema.Table, *schema.Column) (string, error)

	// ConvertHelper returns the statements that create and drop what
	// AlterColumnType's conversion of c's old value calls, or two empty
	// strings when it calls nothing. Nil where the back end has no
	// Column.ConvertFrom, which the planner then refuses.
	ConvertHelper func(*schema.Column) (create, drop string)

	SetDefault  func(*schema.Table, *schema.Column) string
	DropDefault func(*schema.Table, *schema.Column) string

	// FillNulls sets Column.FillNulls on the rows where the column is NULL.
	FillNulls func(*schema.Table, *schema.Column) string

	AddUnique      func(*schema.Table, *schema.Unique) string
	AddCheck       func(*schema.Table, *schema.Check) string
	DropConstraint func(t *schema.Table, name string) string

	CreateIndex func(*schema.Table, *schema.Index) string
	DropIndex   func(*schema.Table, *schema.Index) string

	// CreateIndexConcurrently and DropIndexConcurrently are nil where the back
	// end has no non-blocking index build, and Plan.Concurrently is then the
	// identity.
	//
	// SQL Server's nearest thing is WITH (ONLINE = ON), which is an Enterprise
	// edition feature. Emitting it would produce a migration that works on
	// whichever machine the person writing it uses and fails with "not
	// supported in this edition" on the one it was written for — a worse
	// outcome than not offering it, because the failure arrives during the
	// deployment rather than during review.
	CreateIndexConcurrently func(*schema.Table, *schema.Index) string
	DropIndexConcurrently   func(string) string

	// ValidateConstraint checks the existing rows against a constraint that
	// was added NOT VALID, and NotValid renders a constraint's ADD in that
	// form. Both are nil where the back end has no such split, and then an
	// unvalidated constraint is neither read back nor written.
	//
	// The split is how a constraint reaches a table with traffic: ADD ... NOT
	// VALID holds its lock only as long as it takes to write the catalog, and
	// VALIDATE scans the rows under SHARE UPDATE EXCLUSIVE, which blocks no
	// reader and no writer. Added in one step, the scan runs under the ADD's
	// own lock — for a foreign key, one that blocks every write to the table.
	ValidateConstraint func(t *schema.Table, name string) string
	NotValid           func(add string) string

	// RenameTable and RenameColumn rename in place, which is what keeps the
	// rows: the alternative a differ without them produces is a DROP and an
	// empty CREATE under the new name.
	RenameTable  func(old, new string) string
	RenameColumn func(t *schema.Table, old, new string) string

	// AddPrimaryKey and DropPrimaryKey change a table's key. The drop cannot
	// name the constraint on PostgreSQL or SQL Server: a key declared inline
	// is named by the server (users_pkey, PK__users__3213E83F), and the IR
	// does not carry it, so both renderers read it from the catalogue.
	AddPrimaryKey  func(*schema.Table) string
	DropPrimaryKey func(*schema.Table) string

	// AddExclude adds an exclusion constraint. PostgreSQL-only; every other
	// back end refuses the construct in its Check, so nil there is never
	// reached by a schema that got this far.
	AddExclude func(*schema.Table, *schema.Exclude) string

	// CommentTable and CommentColumn set or clear a comment. Nil where the
	// back end's introspection does not read comments back, since a comment
	// written there would be invisible to the next diff.
	CommentTable  func(*schema.Table) string
	CommentColumn func(*schema.Table, *schema.Column) string

	// DropExpression turns a generated column into an ordinary one that
	// keeps its values (PostgreSQL 13+). Nil where the only way is to drop
	// the column and add it again, which loses them.
	DropExpression func(*schema.Table, *schema.Column) string

	// AddIdentity and DropIdentity make an existing column an identity
	// column or stop it being one. Nil where the server cannot do it in
	// place: SQL Server has no ALTER for IDENTITY at all, and Oracle can drop
	// one but not add one.
	AddIdentity  func(*schema.Table, *schema.Column) []string
	DropIdentity func(*schema.Table, *schema.Column) string

	// AddSerial and DropSerial do the same for a PostgreSQL serial: a
	// sequence the column owns and defaults to.
	AddSerial  func(*schema.Table, *schema.Column) []string
	DropSerial func(*schema.Table, *schema.Column) []string

	// AlterFreesColumn says that ALTER COLUMN refuses a column an index, a
	// key, a CHECK or a DEFAULT depends on, as SQL Server's does (error
	// 5074). The planner then drops each of those before the alter and
	// recreates it after.
	AlterFreesColumn bool
}

// ddlFor returns the renderers for a dialect. enums is the TARGET schema's
// enum declarations, which SQL Server needs to render any column at all: its
// enums have no type of their own, so the labels are what decides the width.
func ddlFor(d Dialect, enums map[string]*schema.Enum) (ddl, error) {
	switch d {
	case "", Postgres:
		return postgresDDL(), nil
	case MSSQL:
		return mssqlDDL(enums), nil
	case Oracle:
		return oracleDDL(enums), nil
	case SQLite:
		return sqliteDDL(enums), nil
	default:
		return ddl{}, fmt.Errorf("migrate: no DDL for dialect %q", d)
	}
}

func enumsOf(s *schema.Schema) map[string]*schema.Enum {
	m := make(map[string]*schema.Enum, len(s.Enums))
	for _, e := range s.Enums {
		m[e.Name] = e
	}
	return m
}

// postgresDDL is the renderer set this package used to inline.
//
// Every function here is the statement that was spelled out at its call site
// before the seam existed, moved and not rewritten: the output is byte for
// byte what it was, which is what makes the refactor reviewable against the
// existing tests rather than against a reading of them.
func postgresDDL() ddl {
	q := pgddl.Ident
	d := ddl{
		Ident: q,

		FillNulls: func(t *schema.Table, c *schema.Column) string { return fillNulls(q, t, c) + ";" },

		CreateTable: func(t *schema.Table) (string, error) {
			return strings.TrimRight(pgddl.CreateTable(t), "\n"), nil
		},
		DropTable:     func(t *schema.Table) string { return "DROP TABLE " + q(t.Name) + ";" },
		AddForeignKey: pgddl.AddForeignKey,

		AddColumn: func(t *schema.Table, c *schema.Column) (string, error) {
			return "ALTER TABLE " + q(t.Name) + " ADD COLUMN " + pgddl.ColumnDef(c) + ";", nil
		},
		DropColumn: func(t *schema.Table, c *schema.Column) string {
			return "ALTER TABLE " + q(t.Name) + " DROP COLUMN " + q(c.Name) + ";"
		},

		AlterColumnType: func(t *schema.Table, c *schema.Column) (string, error) {
			s := "ALTER TABLE " + q(t.Name) + " ALTER COLUMN " + q(c.Name) + " TYPE " + c.Type.SQL()
			if using := pgUsing(c); using != "" {
				s += " USING " + using
			}
			return s + ";", nil
		},
		ConvertHelper: pgConvertHelper,
		SetNotNull: func(t *schema.Table, c *schema.Column) (string, error) {
			return "ALTER TABLE " + q(t.Name) + " ALTER COLUMN " + q(c.Name) + " SET NOT NULL;", nil
		},
		DropNotNull: func(t *schema.Table, c *schema.Column) (string, error) {
			return "ALTER TABLE " + q(t.Name) + " ALTER COLUMN " + q(c.Name) + " DROP NOT NULL;", nil
		},

		SetDefault: func(t *schema.Table, c *schema.Column) string {
			return "ALTER TABLE " + q(t.Name) + " ALTER COLUMN " + q(c.Name) +
				" SET DEFAULT " + c.Default + ";"
		},
		DropDefault: func(t *schema.Table, c *schema.Column) string {
			return "ALTER TABLE " + q(t.Name) + " ALTER COLUMN " + q(c.Name) + " DROP DEFAULT;"
		},

		AddUnique: func(t *schema.Table, u *schema.Unique) string {
			return "ALTER TABLE " + q(t.Name) + " ADD CONSTRAINT " + q(u.Name) +
				" UNIQUE (" + identList(q, u.Columns) + ");"
		},
		AddCheck: func(t *schema.Table, c *schema.Check) string {
			return "ALTER TABLE " + q(t.Name) + " ADD CONSTRAINT " + q(c.Name) +
				" CHECK (" + c.Expr + ");"
		},
		DropConstraint: func(t *schema.Table, name string) string {
			return "ALTER TABLE " + q(t.Name) + " DROP CONSTRAINT " + q(name) + ";"
		},

		CreateIndex: pgddl.CreateIndex,
		DropIndex: func(_ *schema.Table, ix *schema.Index) string {
			return "DROP INDEX " + q(ix.Name) + ";"
		},

		CreateIndexConcurrently: pgddl.CreateIndexConcurrently,
		DropIndexConcurrently:   pgddl.DropIndexConcurrently,

		ValidateConstraint: func(t *schema.Table, name string) string {
			return "ALTER TABLE " + q(t.Name) + " VALIDATE CONSTRAINT " + q(name) + ";"
		},
		NotValid: func(add string) string {
			return strings.TrimSuffix(strings.TrimRight(add, "\n"), ";") + " NOT VALID;"
		},

		RenameTable: func(old, new string) string {
			return "ALTER TABLE " + q(old) + " RENAME TO " + q(new) + ";"
		},
		RenameColumn: func(t *schema.Table, old, new string) string {
			return "ALTER TABLE " + q(t.Name) + " RENAME COLUMN " + q(old) + " TO " + q(new) + ";"
		},

		AddPrimaryKey: func(t *schema.Table) string {
			return "ALTER TABLE " + q(t.Name) + " ADD PRIMARY KEY (" + identList(q, t.PrimaryKey) + ");"
		},
		DropPrimaryKey: func(t *schema.Table) string {
			// PostgreSQL has no DROP PRIMARY KEY, and the constraint's name
			// is whatever the server or the database's author chose.
			return "DO $storm$\nDECLARE n text;\nBEGIN\n" +
				"  SELECT conname INTO n FROM pg_constraint WHERE conrelid = " + quote(q(t.Name)) +
				"::regclass AND contype = 'p';\n" +
				"  IF n IS NOT NULL THEN EXECUTE " + quote("ALTER TABLE "+q(t.Name)+" DROP CONSTRAINT ") +
				" || quote_ident(n); END IF;\nEND $storm$;"
		},

		AddExclude: pgddl.AddExclude,

		CommentTable: func(t *schema.Table) string {
			return "COMMENT ON TABLE " + q(t.Name) + " IS " + commentLit(t.Comment) + ";"
		},
		CommentColumn: func(t *schema.Table, c *schema.Column) string {
			return "COMMENT ON COLUMN " + q(t.Name) + "." + q(c.Name) + " IS " + commentLit(c.Comment) + ";"
		},

		DropExpression: func(t *schema.Table, c *schema.Column) string {
			return "ALTER TABLE " + q(t.Name) + " ALTER COLUMN " + q(c.Name) + " DROP EXPRESSION;"
		},

		AddIdentity: func(t *schema.Table, c *schema.Column) []string {
			// The sequence an identity creates starts at 1, and the column
			// already holds values: without the setval the next INSERT
			// collides with the first row.
			return []string{
				"ALTER TABLE " + q(t.Name) + " ALTER COLUMN " + q(c.Name) + " ADD GENERATED BY DEFAULT AS IDENTITY;",
				pgResync(t, c, "pg_get_serial_sequence("+quote(q(t.Name))+", "+quote(c.Name)+")"),
			}
		},
		DropIdentity: func(t *schema.Table, c *schema.Column) string {
			return "ALTER TABLE " + q(t.Name) + " ALTER COLUMN " + q(c.Name) + " DROP IDENTITY;"
		},
		AddSerial: func(t *schema.Table, c *schema.Column) []string {
			// What `serial` does at CREATE TABLE, spelled out: a sequence of
			// the column's width, owned by it so it goes when the column does,
			// named as PostgreSQL would name it, and moved past the rows.
			seq := q(t.Name + "_" + c.Name + "_seq")
			return []string{
				"CREATE SEQUENCE " + seq + " AS " + pgIntName(c.Type) + " OWNED BY " + q(t.Name) + "." + q(c.Name) + ";",
				"ALTER TABLE " + q(t.Name) + " ALTER COLUMN " + q(c.Name) + " SET DEFAULT nextval(" + quote(seq) + ");",
				pgResync(t, c, quote(seq)),
			}
		},
		DropSerial: func(t *schema.Table, c *schema.Column) []string {
			// The default first: it is what depends on the sequence.
			return []string{
				"ALTER TABLE " + q(t.Name) + " ALTER COLUMN " + q(c.Name) + " DROP DEFAULT;",
				"DO $storm$\nDECLARE s text := pg_get_serial_sequence(" + quote(q(t.Name)) + ", " + quote(c.Name) + ");\n" +
					"BEGIN\n  IF s IS NOT NULL THEN EXECUTE 'DROP SEQUENCE ' || s; END IF;\nEND $storm$;",
			}
		},
	}

	d.Enums = func(p *Plan, from, to *schema.Schema) error {
		// Enums before tables that use them.
		for _, e := range to.Enums {
			old := from.Enum(e.Name)
			if old == nil {
				p.add(Change{SQL: pgddl.CreateEnum(e)})
				continue
			}
			// Labels can be added but never removed or reordered in place. A
			// label is added WHERE the model puts it: an enum's order is its
			// sort order, and one appended at the end of a model that put it
			// in the middle sorts every row differently from the model.
			for i, l := range e.Labels {
				if contains(old.Labels, l) {
					continue
				}
				at := ""
				switch {
				case i == len(e.Labels)-1:
				case i > 0:
					at = " AFTER " + quote(e.Labels[i-1])
				default:
					// The first label goes before the first one that is
					// already there; the labels after it follow it.
					for _, next := range e.Labels[1:] {
						if contains(old.Labels, next) {
							at = " BEFORE " + quote(next)
							break
						}
					}
				}
				p.add(Change{
					SQL: fmt.Sprintf("ALTER TYPE %s ADD VALUE %s%s;", q(e.Name), quote(l), at),

					addsEnumValue: true,
				})
			}
			if !sameOrder(old.Labels, e.Labels) {
				p.add(Change{
					SQL:         fmt.Sprintf("-- cannot reorder the labels of %s: PostgreSQL has no ALTER TYPE ... reorder", e.Name),
					Destructive: true,
					Why:         "enum " + e.Name + " lists its labels in a different order in the model; recreate the type by hand",
				})
			}
			for _, l := range old.Labels {
				if !contains(e.Labels, l) {
					p.add(Change{
						SQL:         fmt.Sprintf("-- cannot remove enum label %s from %s: PostgreSQL has no DROP VALUE", quote(l), e.Name),
						Destructive: true,
						Why:         "enum label " + l + " removed from the model; recreate the type by hand",
					})
				}
			}
		}
		return nil
	}
	d.DropEnums = func(p *Plan, from, to *schema.Schema) {
		for _, e := range from.Enums {
			if to.Enum(e.Name) == nil {
				p.add(Change{
					SQL:         "DROP TYPE " + q(e.Name) + ";",
					Destructive: true,
					Why:         "enum " + e.Name + " is no longer in the model",
				})
			}
		}
	}
	return d
}

// sameOrder reports whether the labels a and b share appear in the same
// order in both.
func sameOrder(a, b []string) bool {
	var x, y []string
	for _, l := range a {
		if contains(b, l) {
			x = append(x, l)
		}
	}
	for _, l := range b {
		if contains(a, l) {
			y = append(y, l)
		}
	}
	return eq(x, y)
}

// commentLit is a comment's literal, or NULL — which is how PostgreSQL and
// Oracle remove one — for an empty comment.
func commentLit(s string) string {
	if s == "" {
		return "NULL"
	}
	return quote(s)
}

// pgResync moves seq past every value the column already holds.
func pgResync(t *schema.Table, c *schema.Column, seq string) string {
	q := pgddl.Ident
	return "SELECT setval(" + seq + ", coalesce(max(" + q(c.Name) + "), 0) + 1, false) FROM " + q(t.Name) + ";"
}

// pgIntName is the SQL name of an integer type, for CREATE SEQUENCE ... AS.
func pgIntName(t schema.Type) string {
	switch t.Name {
	case schema.TypeInt2:
		return "smallint"
	case schema.TypeInt4:
		return "integer"
	default:
		return "bigint"
	}
}

// pgUsing is the USING clause that converts c's old value to its type, or ""
// for PostgreSQL's own cast.
//
// For ConvertJSONText, ” and the JSON null become the column's default,
// and anything else is parsed as JSON: a json or jsonb column takes the
// document as it is, and an array column its elements through the helper
// pgConvertHelper creates, since USING refuses a subquery and only a
// function can unnest. A value that is not JSON, or not an array for an
// array column, fails the statement, and so the migration with nothing
// applied.
func pgUsing(c *schema.Column) string {
	if c.ConvertFrom != schema.ConvertJSONText {
		return ""
	}
	src := "nullif(nullif(btrim(" + pgddl.Ident(c.Name) + "::text), ''), 'null')"
	fallback := "NULL"
	if c.Default != "" {
		fallback = "(" + c.Default + ")::" + c.Type.SQL()
	}
	switch {
	case c.Type.Array:
		return "coalesce(" + pgJSONArrayFunc(c.Type) + "(" + src + "), " + fallback + ")"
	case c.Type.Name == schema.TypeJSON || c.Type.Name == schema.TypeJSONB:
		return "coalesce(" + src + "::" + c.Type.SQL() + ", " + fallback + ")"
	default:
		return "" // the builder refuses ConvertFrom on any other type
	}
}

// pgConvertHelper creates and drops the function pgUsing calls for an array
// column: one per element type, in pg_temp, so it is private to the session
// running the migration and leaves nothing in the schema should the drop
// never run.
func pgConvertHelper(c *schema.Column) (create, drop string) {
	if c.ConvertFrom != schema.ConvertJSONText || !c.Type.Array {
		return "", ""
	}
	fn := pgJSONArrayFunc(c.Type)
	elem := c.Type
	elem.Array = false
	create = "CREATE OR REPLACE FUNCTION " + fn + "(s text) RETURNS " + c.Type.SQL() +
		" LANGUAGE sql IMMUTABLE STRICT AS $storm$\n" +
		"    SELECT coalesce(array_agg(e::" + elem.SQL() + " ORDER BY n), '{}')\n" +
		"      FROM jsonb_array_elements_text(s::jsonb) WITH ORDINALITY AS x(e, n)\n" +
		"$storm$;"
	return create, "DROP FUNCTION " + fn + "(text);"
}

// pgJSONArrayFunc names the helper that reads JSON text into an array of t's
// element type.
func pgJSONArrayFunc(t schema.Type) string {
	elem := t
	elem.Array = false
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, strings.ToLower(elem.SQL()))
	return "pg_temp.storm_json_" + name
}

// fillNulls is the UPDATE that gives a column's NULL rows Column.FillNulls,
// the same statement on every back end but for how a name is quoted.
func fillNulls(q func(string) string, t *schema.Table, c *schema.Column) string {
	return "UPDATE " + q(t.Name) + " SET " + q(c.Name) + " = " + c.FillNulls + " WHERE " + q(c.Name) + " IS NULL"
}
