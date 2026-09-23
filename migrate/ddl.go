package migrate

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/schema"
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

	SetDefault  func(*schema.Table, *schema.Column) string
	DropDefault func(*schema.Table, *schema.Column) string

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
			return "ALTER TABLE " + q(t.Name) + " ALTER COLUMN " + q(c.Name) +
				" TYPE " + c.Type.SQL() + ";", nil
		},
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
	}

	d.Enums = func(p *Plan, from, to *schema.Schema) error {
		// Enums before tables that use them.
		for _, e := range to.Enums {
			old := from.Enum(e.Name)
			if old == nil {
				p.add(Change{SQL: pgddl.CreateEnum(e)})
				continue
			}
			// Labels can be appended but never removed or reordered in place.
			for _, l := range e.Labels {
				if !contains(old.Labels, l) {
					p.add(Change{
						SQL: fmt.Sprintf("ALTER TYPE %s ADD VALUE %s;", q(e.Name), quote(l)),

						addsEnumValue: true,
					})
				}
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
