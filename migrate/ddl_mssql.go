package migrate

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gsoultan/storm/compile/msddl"
	"github.com/gsoultan/storm/schema"
)

// The SQL Server half of the seam.
//
// Most of it is the same statement with different punctuation. Five are not,
// and each is commented where it is rendered:
//
//  1. ADD COLUMN is ADD. The word COLUMN is a syntax error.
//  2. ALTER COLUMN restates the type AND the nullability every time. There is
//     no SET NOT NULL and no ALTER COLUMN ... TYPE: one clause carries both
//     facts, so all three of the seam's column-alter renderers are one
//     function here.
//  3. A DEFAULT is a named CONSTRAINT, not a property of the column. Dropping
//     one means naming it, and a default storm did not create is named
//     DF__users__status__7A672E12 by the server.
//  4. DROP INDEX names the table.
//  5. There is no enum, so there is no enum step. See Enums below.
func mssqlDDL(enums map[string]*schema.Enum) ddl {
	q := msddl.Ident

	// One function for all three column alters: see (2) above. Nullability is
	// always spelled out, never left off — SQL Server's behaviour for an
	// ALTER COLUMN that omits NULL/NOT NULL depends on ANSI_NULL_DFLT_ON, a
	// session setting, so a statement that leaves it out means different
	// things to different clients.
	alterColumn := func(t *schema.Table, c *schema.Column) (string, error) {
		ty, err := msddl.ColumnType(t.Name, c, enums)
		if err != nil {
			return "", err
		}
		nullability := " NULL"
		if c.NotNull {
			nullability = " NOT NULL"
		}
		return "ALTER TABLE " + q(t.Name) + " ALTER COLUMN " + q(c.Name) + " " + ty + nullability + ";", nil
	}

	d := ddl{
		Ident: q,

		// The table AND its indexes, because msddl.CreateTable is only the
		// CREATE TABLE: msddl.Create emits the indexes in a second pass over
		// the schema, which a diff building one table at a time never reaches.
		// pgddl.CreateTable appends them itself, so the seam's two halves
		// agree on what "create this table" means; without this a new table
		// would arrive with none of its indexes and the NEXT diff would
		// propose to add them.
		CreateTable: func(t *schema.Table) (string, error) {
			def, err := msddl.CreateTable(t, enums)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			b.WriteString(def)
			for _, ix := range t.Indexes {
				b.WriteString("\n")
				b.WriteString(msddl.CreateIndex(t, ix))
			}
			return b.String(), nil
		},
		DropTable:     func(t *schema.Table) string { return "DROP TABLE " + q(t.Name) + ";" },
		AddForeignKey: msddl.AddForeignKey,

		AddColumn: func(t *schema.Table, c *schema.Column) (string, error) {
			def, err := msddl.ColumnDef(t.Name, c, enums)
			if err != nil {
				return "", err
			}
			return "ALTER TABLE " + q(t.Name) + " ADD " + def + ";", nil // (1)
		},
		DropColumn: func(t *schema.Table, c *schema.Column) string {
			return "ALTER TABLE " + q(t.Name) + " DROP COLUMN " + q(c.Name) + ";"
		},

		AlterColumnType: alterColumn,
		SetNotNull:      alterColumn,
		DropNotNull:     alterColumn,

		SetDefault: func(t *schema.Table, c *schema.Column) string {
			// (3) Two statements, because setting a default here is replacing
			// a constraint: ADD CONSTRAINT ... DEFAULT on a column that
			// already has one is error 1781 ("column already has a DEFAULT
			// bound to it"), not a replacement. The drop is guarded on the
			// catalogue, so it costs nothing when there was no old default —
			// which is why the diff can treat this as one change.
			//
			// The new one is NAMED, unlike the inline DEFAULT that
			// msddl.CreateTable emits: a constraint added by a migration is
			// one somebody will have to drop by hand one day, and
			// DF_users_status reads better in that moment than the server's
			// DF__users__status__7A672E12.
			return dropDefaultSQL(t, c) + "\n" +
				"ALTER TABLE " + q(t.Name) + " ADD CONSTRAINT " + q(defaultName(t.Name, c.Name)) +
				" DEFAULT (" + c.Default + ") FOR " + q(c.Name) + ";"
		},
		DropDefault: dropDefaultSQL,

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

		CreateIndex: msddl.CreateIndex,
		DropIndex: func(t *schema.Table, ix *schema.Index) string {
			// (4) The table is not optional. `DROP INDEX t.ix` is the
			// deprecated form and `DROP INDEX ix` alone is a syntax error,
			// which is why the seam hands the table to a renderer that
			// PostgreSQL does not need it for.
			return "DROP INDEX " + q(ix.Name) + " ON " + q(t.Name) + ";"
		},

		// CreateIndexConcurrently and DropIndexConcurrently stay nil. See the
		// field comment: ONLINE = ON is Enterprise-only.
	}

	d.Enums = func(_ *Plan, from, to *schema.Schema) error {
		// (5) SQL Server has no enum type. msddl renders the labels as a CHECK
		// constraint named ck_<table>_<column>, which travels with the table
		// and is therefore diffed as an ordinary check — there is nothing here
		// for a plan to CREATE or DROP.
		//
		// But a schema that still DECLARES enums has not been through
		// NormalizeMSSQL, so its enum columns are typed as the enum rather
		// than as the nvarchar the server stores, and its enum CHECKs are not
		// in Table.Checks. Diffing that against a live database would propose
		// to retype every enum column and to drop every enum constraint.
		// Refuse it rather than emit it.
		if len(from.Enums) == 0 && len(to.Enums) == 0 {
			return nil
		}
		return errors.New("migrate: schema still declares enums, which SQL Server has no type for: " +
			"normalise the model with NormalizeMSSQL (or storm diff against a live server) so the " +
			"labels are compared as the CHECK constraints the server actually stores")
	}
	// DropEnums stays nil: there is no type to drop.
	return d
}

// defaultName is what a DEFAULT constraint this package adds is called.
// Derived rather than declared, for the same reason msddl derives the enum
// check's name: the model has nowhere to put it, and a plan that re-renders
// has to arrive at the same name twice.
func defaultName(table, col string) string { return "DF_" + table + "_" + col }

// dropDefaultSQL drops whatever default constraint a column happens to have.
//
// It cannot name the constraint, because it does not know it. A default storm
// itself wrote from CreateTable is inline and unnamed, so the server invented
// something like DF__users__status__7A672E12; a default in a database storm is
// being pointed at for the first time could be called anything at all. The
// catalogue is the only place the name exists, so the statement reads it.
//
// The variable holds the WHOLE statement rather than the name, because EXEC's
// argument may only be variables and string literals concatenated — a function
// call inside it is a syntax error, and QUOTENAME is a function call. So the
// quoting happens in the SELECT and EXEC gets one variable.
//
// The variable is named after the table and column rather than something
// short, because DECLARE is scoped to the BATCH, not to a block: a migration
// that drops two defaults is two of these concatenated, and a second
// `DECLARE @n` in the same batch is an error.
func dropDefaultSQL(t *schema.Table, c *schema.Column) string {
	// The table's LENGTH is in the name because sanitizing loses information:
	// table `a-b` column `c` and table `a` column `b_c` both flatten to
	// `a_b_c`, and two DECLAREs of one name in a batch is an error mid-migration.
	v := fmt.Sprintf("@storm_df_%d_%s_%s", len(t.Name), sanitizeVar(t.Name), sanitizeVar(c.Name))
	tbl := msddl.Ident(t.Name)
	return "DECLARE " + v + " nvarchar(max);\n" +
		"SELECT " + v + " = N'ALTER TABLE " + tbl + " DROP CONSTRAINT ' + QUOTENAME(dc.name)\n" +
		"  FROM sys.default_constraints dc\n" +
		"  JOIN sys.columns c ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id\n" +
		" WHERE dc.parent_object_id = OBJECT_ID(N" + quote(tbl) + ") AND c.name = N" + quote(c.Name) + ";\n" +
		"IF " + v + " IS NOT NULL EXEC(" + v + ");"
}

// sanitizeVar makes an identifier fragment safe to paste into a variable name.
// Names reaching here come from a model or from a catalogue, not from a
// request, but a table called `order-item` would otherwise produce a statement
// that does not parse.
func sanitizeVar(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
