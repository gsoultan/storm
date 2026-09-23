package migrate

import (
	"errors"
	"strings"

	"github.com/gsoultan/storm/compile/oraddl"
	"github.com/gsoultan/storm/schema"
)

// The Oracle half of the seam.
//
// Closer to SQL Server's than to PostgreSQL's, and the four that differ from
// BOTH are commented where they are rendered:
//
//  1. ADD COLUMN is ADD, as on SQL Server — but `ALTER TABLE t ADD (a, b)`
//     takes a PARENTHESISED list, and the single-column form without
//     parentheses is also legal. storm emits one column at a time, so the
//     bare form is the one used.
//  2. ALTER COLUMN is MODIFY, and it restates only what CHANGES. SQL Server's
//     restates the type on every change, including one that is only about
//     nullability; Oracle's does the opposite — `MODIFY (c NOT NULL)` is
//     legal and leaves the type alone, and repeating the type when it has not
//     changed is ORA-01442 ("column to be modified to NOT NULL is already NOT
//     NULL") in some orders.
//  3. A DEFAULT is a property of the COLUMN, as on PostgreSQL — not a named
//     constraint as on SQL Server. `MODIFY (c DEFAULT NULL)` drops it, and
//     there is no catalogue lookup to do.
//  4. There is no terminating semicolon. Through the protocol a trailing `;`
//     is ORA-00911 — see compile/oraddl.Statements, which is the primitive for
//     the same reason.
func oracleDDL(enums map[string]*schema.Enum) ddl {
	q := oraddl.Ident

	// (2) MODIFY restates only what changed, so the three column alters are
	// three different statements here — unlike SQL Server, where they are one.
	modify := func(t *schema.Table, body string) string {
		return "ALTER TABLE " + q(t.Name) + " MODIFY (" + body + ")"
	}

	d := ddl{
		Ident: q,

		CreateTable: func(t *schema.Table) (string, error) {
			def, err := oraddl.CreateTable(t, enums)
			if err != nil {
				return "", err
			}
			// The table AND its indexes, for the reason msddl's needs the
			// same: oraddl.Statements emits indexes in a second pass over the
			// schema, which a diff building one table at a time never reaches.
			var b strings.Builder
			b.WriteString(def)
			for _, ix := range t.Indexes {
				b.WriteString("\n")
				b.WriteString(oraddl.CreateIndex(t, ix))
			}
			return b.String(), nil
		},
		DropTable: func(t *schema.Table) string {
			// CASCADE CONSTRAINTS, because a foreign key pointing AT this
			// table is not dropped by dropping the table and ORA-02449
			// refuses. PURGE, because without it the table goes to the
			// recycle bin under a system name and a re-created one collides
			// with its indexes.
			return "DROP TABLE " + q(t.Name) + " CASCADE CONSTRAINTS PURGE"
		},
		AddForeignKey: oraddl.AddForeignKey,

		AddColumn: func(t *schema.Table, c *schema.Column) (string, error) {
			def, err := oraddl.ColumnDef(t.Name, c, enums)
			if err != nil {
				return "", err
			}
			return "ALTER TABLE " + q(t.Name) + " ADD " + def, nil // (1)
		},
		DropColumn: func(t *schema.Table, c *schema.Column) string {
			return "ALTER TABLE " + q(t.Name) + " DROP COLUMN " + q(c.Name)
		},

		// (2) The type alone. Nullability is NOT restated: repeating a NOT
		// NULL a column already has is ORA-01442, and MODIFY leaves untouched
		// what it does not mention.
		AlterColumnType: func(t *schema.Table, c *schema.Column) (string, error) {
			ty, err := oraddl.ColumnType(t.Name, c, enums)
			if err != nil {
				return "", err
			}
			return modify(t, q(c.Name)+" "+ty), nil
		},
		SetNotNull: func(t *schema.Table, c *schema.Column) (string, error) {
			return modify(t, q(c.Name)+" NOT NULL"), nil
		},
		DropNotNull: func(t *schema.Table, c *schema.Column) (string, error) {
			return modify(t, q(c.Name)+" NULL"), nil
		},

		// (3) A property of the column, so there is no constraint to name and
		// no catalogue to search — the thing SQL Server's DropDefault needs a
		// whole PL/SQL-shaped statement for.
		SetDefault: func(t *schema.Table, c *schema.Column) string {
			return modify(t, q(c.Name)+" DEFAULT "+c.Default)
		},
		DropDefault: func(t *schema.Table, c *schema.Column) string {
			return modify(t, q(c.Name)+" DEFAULT NULL")
		},

		AddUnique: func(t *schema.Table, u *schema.Unique) string {
			return "ALTER TABLE " + q(t.Name) + " ADD CONSTRAINT " + q(u.Name) +
				" UNIQUE (" + identList(q, u.Columns) + ")"
		},
		AddCheck: func(t *schema.Table, c *schema.Check) string {
			return "ALTER TABLE " + q(t.Name) + " ADD CONSTRAINT " + q(c.Name) +
				" CHECK (" + c.Expr + ")"
		},
		DropConstraint: func(t *schema.Table, name string) string {
			return "ALTER TABLE " + q(t.Name) + " DROP CONSTRAINT " + q(name)
		},

		CreateIndex: oraddl.CreateIndex,
		DropIndex: func(_ *schema.Table, ix *schema.Index) string {
			// No ON clause: an index name is unique per SCHEMA here, not per
			// table, which is why SQL Server needs the table and this does
			// not — and why two tables in one schema cannot share an index
			// name, something oraddl's derived names already respect.
			return "DROP INDEX " + q(ix.Name)
		},

		// CreateIndexConcurrently and DropIndexConcurrently stay nil. Oracle's
		// ONLINE index build is an Enterprise Edition feature, exactly as SQL
		// Server's WITH (ONLINE = ON) is — a migration that works on the
		// machine it was written on and fails with "feature not enabled" on
		// the one it was written for is worse than not offering it.
	}

	d.Enums = func(_ *Plan, from, to *schema.Schema) error {
		// Oracle has no enum type either: oraddl renders the labels as a CHECK
		// named ck_<table>_<column>, which travels with the table and is
		// diffed as an ordinary check. A schema that still DECLARES enums has
		// not been normalised, and diffing it would propose to retype every
		// enum column and drop every enum CHECK.
		if len(from.Enums) == 0 && len(to.Enums) == 0 {
			return nil
		}
		return errors.New("migrate: schema still declares enums, which Oracle has no type " +
			"for: normalise the model with NormalizeOracle (or storm diff against a live " +
			"server) so the labels are compared as the CHECK constraints the server stores")
	}
	// DropEnums stays nil: there is no type to drop.
	return d
}
