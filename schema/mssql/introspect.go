// Package mssql reads a live SQL Server database into storm's IR.
//
// The on-ramp for an existing database: `storm import` turns what is already
// there into a Go MODEL, because storm is model-first and adopting a database
// with forty tables means having a model to start from. Hand-writing one is
// where adoption stops.
//
// It is the second introspector, and the differences from schema/pg are about
// what the two servers KEEP rather than about SQL:
//
//   - There is no enum type, so there is nothing to read back. storm's enums
//     become an NVARCHAR and a CHECK here, and turning `status IN ('new',
//     'paid')` back into a declared label set would be pattern-matching a
//     constraint somebody may have written by hand for another reason. The
//     CHECK is imported as a check; the enum-ness is not invented.
//   - A default is stored as TEXT with its own parentheses — `((0))`,
//     `(newid())`, `('new')` — and the layers are the server's, not the
//     model's. They are peeled, or every imported default arrives wrapped in
//     brackets the model never wrote.
//   - A column's max_length is in BYTES, and an nvarchar's characters are two
//     of them. Reading it as a character count halves every imported width,
//     which is a model that compiles and truncates.
package mssql

import (
	"context"
	"fmt"

	"strings"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/msdec"
	"github.com/gsoultan/storm/schema"
)

// Conn is the slice of a driver this package needs. runtime.Executor satisfies
// it, which means a *msdrv.Pool and a *msdrv.Conn both do.
type Conn interface {
	Query(ctx context.Context, sql string, args []any) (runtime.Rows, error)
}

// Introspect reads one schema — usually "dbo" — into the IR.
func Introspect(ctx context.Context, c Conn, namespace string) (*schema.Schema, error) {
	if namespace == "" {
		namespace = "dbo"
	}
	s := &schema.Schema{}
	byName := map[string]*schema.Table{}

	for _, step := range []struct {
		what string
		load func(context.Context, Conn, string, *schema.Schema, map[string]*schema.Table) error
	}{
		{"tables", loadTables},
		{"columns", loadColumns},
		{"constraints", loadKeys},
		{"checks", loadChecks},
		{"indexes", loadIndexes},
		{"foreign keys", loadForeignKeys},
	} {
		if err := step.load(ctx, c, namespace, s, byName); err != nil {
			return nil, fmt.Errorf("%s: %w", step.what, err)
		}
	}
	s.Normalize()
	return s, nil
}

// rowReader walks a result set, decoding the columns this package asks for.
//
// A small helper rather than a scanner per query: every statement below reads
// names and definitions, which are nvarchar, and a handful of integers. The
// generated scanners exist for a model's own rows; catalogue reads are cold and
// are clearer as a loop.
type rowReader struct {
	rows runtime.Rows
	sl   runtime.Slab
	v    [][]byte
}

func query(ctx context.Context, c Conn, sql string, args ...any) (*rowReader, error) {
	rows, err := c.Query(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	return &rowReader{rows: rows}, nil
}

func (r *rowReader) next() bool {
	if !r.rows.Next() {
		return false
	}
	r.v = r.rows.RawValues()
	return true
}

func (r *rowReader) close() error { r.rows.Close(); return r.rows.Err() }

// str decodes an nvarchar column. NULL is the empty string: every text column
// this package reads is a name or a definition, and "absent" and "empty" mean
// the same thing for both.
func (r *rowReader) str(i int) string {
	if i >= len(r.v) || r.v[i] == nil {
		return ""
	}
	return msdec.Str(r.v[i], &r.sl)
}

func (r *rowReader) int(i int) int64 {
	if i >= len(r.v) || r.v[i] == nil {
		return 0
	}
	switch len(r.v[i]) {
	case 1:
		return int64(msdec.Int1(r.v[i]))
	case 2:
		return int64(msdec.Int2(r.v[i]))
	case 4:
		return int64(msdec.Int4(r.v[i]))
	}
	return msdec.Int8(r.v[i])
}

func (r *rowReader) bool(i int) bool { return r.int(i) != 0 }

func loadTables(ctx context.Context, c Conn, ns string, s *schema.Schema,
	byName map[string]*schema.Table) error {
	r, err := query(ctx, c, `
		SELECT t.name
		FROM sys.tables t
		JOIN sys.schemas s ON s.schema_id = t.schema_id
		WHERE s.name = @p1 AND t.is_ms_shipped = 0
		ORDER BY t.name`, ns)
	if err != nil {
		return err
	}
	for r.next() {
		t := &schema.Table{Name: r.str(0)}
		s.Tables = append(s.Tables, t)
		byName[t.Name] = t
	}
	return r.close()
}

func loadColumns(ctx context.Context, c Conn, ns string, s *schema.Schema,
	byName map[string]*schema.Table) error {
	r, err := query(ctx, c, `
		SELECT t.name, c.name, ty.name, c.max_length, c.precision, c.scale,
		       c.is_nullable, c.is_identity,
		       COALESCE(dc.definition, N''), COALESCE(cc.definition, N'')
		FROM sys.tables t
		JOIN sys.schemas s ON s.schema_id = t.schema_id
		JOIN sys.columns c ON c.object_id = t.object_id
		JOIN sys.types ty ON ty.user_type_id = c.user_type_id
		LEFT JOIN sys.default_constraints dc ON dc.object_id = c.default_object_id
		LEFT JOIN sys.computed_columns cc
		       ON cc.object_id = c.object_id AND cc.column_id = c.column_id
		WHERE s.name = @p1 AND t.is_ms_shipped = 0
		ORDER BY t.name, c.column_id`, ns)
	if err != nil {
		return err
	}
	for r.next() {
		t := byName[r.str(0)]
		if t == nil {
			continue
		}
		col := &schema.Column{
			Name:      r.str(1),
			Type:      typeOf(r.str(2), r.int(3), r.int(4), r.int(5)),
			NotNull:   !r.bool(6),
			Identity:  r.bool(7),
			Default:   unwrap(r.str(8)),
			Generated: unwrap(r.str(9)),
		}
		t.Columns = append(t.Columns, col)
	}
	return r.close()
}

// typeOf maps a SQL Server type back onto storm's.
//
// The inverse of compile/msddl.TypeSQL, and deliberately not its mirror image:
// several SQL Server types map onto ONE storm type — nchar, nvarchar and
// varchar are all text — so a round trip through import and back out is not
// byte-identical DDL, and is not meant to be. What it has to preserve is the
// MEANING: a width, a precision, whether it is a string or a number.
func typeOf(name string, maxLen, prec, scale int64) schema.Type {
	switch strings.ToLower(name) {
	case "bit":
		return schema.Type{Name: schema.TypeBool}
	case "tinyint", "smallint":
		return schema.Type{Name: schema.TypeInt2}
	case "int":
		return schema.Type{Name: schema.TypeInt4}
	case "bigint":
		return schema.Type{Name: schema.TypeInt8}
	case "real":
		return schema.Type{Name: schema.TypeFloat4}
	case "float":
		// float(24) IS real; the server stores the mantissa bits in precision.
		if prec > 0 && prec <= 24 {
			return schema.Type{Name: schema.TypeFloat4}
		}
		return schema.Type{Name: schema.TypeFloat8}
	case "decimal", "numeric":
		return schema.Type{Name: schema.TypeNumeric, Precision: int(prec), Scale: int(scale)}
	case "money":
		return schema.Type{Name: schema.TypeNumeric, Precision: 19, Scale: 4}
	case "smallmoney":
		return schema.Type{Name: schema.TypeNumeric, Precision: 10, Scale: 4}
	case "uniqueidentifier":
		return schema.Type{Name: schema.TypeUUID}
	case "datetimeoffset":
		return schema.Type{Name: schema.TypeTimestamptz}
	case "datetime", "datetime2", "smalldatetime":
		return schema.Type{Name: schema.TypeTimestamp}
	case "date":
		return schema.Type{Name: schema.TypeDate}
	case "time":
		return schema.Type{Name: schema.TypeTime}
	case "binary", "varbinary", "image", "timestamp", "rowversion":
		return schema.Type{Name: schema.TypeBytea}
	case "text", "ntext", "xml":
		return schema.Type{Name: schema.TypeText}
	case "char", "varchar", "nchar", "nvarchar", "sysname":
		// max_length is BYTES, and -1 is MAX. An n-prefixed type is UTF-16, so
		// its character count is half the bytes — reading it as characters
		// halves every imported width, which is a model that compiles and
		// truncates.
		if maxLen < 0 {
			return schema.Type{Name: schema.TypeText}
		}
		size := maxLen
		if n := strings.ToLower(name); n == "nchar" || n == "nvarchar" || n == "sysname" {
			size /= 2
		}
		return schema.Type{Name: schema.TypeVarchar, Size: int(size)}
	}
	// Unknown: kept as TEXT rather than dropped, so the column survives the
	// import and the model's author sees a field to correct.
	return schema.Type{Name: schema.TypeText}
}

// unwrap peels the parentheses SQL Server stores a definition inside.
//
// It records `DEFAULT 0` as `((0))` and `DEFAULT newid()` as `(newid())` — the
// layers are the catalogue's, not the model's, and importing them verbatim puts
// brackets into a model nobody wrote. One layer is always the server's; the
// rest are only stripped while they WRAP the whole expression, so `(a) + (b)`
// keeps both.
func unwrap(s string) string {
	s = strings.TrimSpace(s)
	for len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' && wraps(s) {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// wraps reports whether the outer parentheses enclose the WHOLE expression
// rather than two halves of it.
func wraps(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				return false
			}
		}
	}
	return depth == 0
}

func loadKeys(ctx context.Context, c Conn, ns string, s *schema.Schema,
	byName map[string]*schema.Table) error {
	r, err := query(ctx, c, `
		SELECT t.name, kc.name, i.is_primary_key, col.name
		FROM sys.key_constraints kc
		JOIN sys.tables t ON t.object_id = kc.parent_object_id
		JOIN sys.schemas s ON s.schema_id = t.schema_id
		JOIN sys.indexes i
		     ON i.object_id = kc.parent_object_id AND i.index_id = kc.unique_index_id
		JOIN sys.index_columns ic
		     ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		JOIN sys.columns col
		     ON col.object_id = ic.object_id AND col.column_id = ic.column_id
		WHERE s.name = @p1 AND ic.is_included_column = 0
		ORDER BY t.name, kc.name, ic.key_ordinal`, ns)
	if err != nil {
		return err
	}
	type key struct {
		table, name string
	}
	uniques := map[key]*schema.Unique{}
	for r.next() {
		t := byName[r.str(0)]
		if t == nil {
			continue
		}
		if r.bool(2) {
			t.PrimaryKey = append(t.PrimaryKey, r.str(3))
			continue
		}
		k := key{r.str(0), r.str(1)}
		u := uniques[k]
		if u == nil {
			u = &schema.Unique{Name: k.name}
			uniques[k] = u
			t.Uniques = append(t.Uniques, u)
		}
		u.Columns = append(u.Columns, r.str(3))
	}
	return r.close()
}

func loadChecks(ctx context.Context, c Conn, ns string, s *schema.Schema,
	byName map[string]*schema.Table) error {
	r, err := query(ctx, c, `
		SELECT t.name, cc.name, cc.definition
		FROM sys.check_constraints cc
		JOIN sys.tables t ON t.object_id = cc.parent_object_id
		JOIN sys.schemas s ON s.schema_id = t.schema_id
		WHERE s.name = @p1
		ORDER BY t.name, cc.name`, ns)
	if err != nil {
		return err
	}
	for r.next() {
		t := byName[r.str(0)]
		if t == nil {
			continue
		}
		t.Checks = append(t.Checks, &schema.Check{
			Name: r.str(1), Expr: unwrap(r.str(2)),
		})
	}
	return r.close()
}

func loadIndexes(ctx context.Context, c Conn, ns string, s *schema.Schema,
	byName map[string]*schema.Table) error {
	r, err := query(ctx, c, `
		SELECT t.name, i.name, i.is_unique, COALESCE(i.filter_definition, N''),
		       col.name, ic.is_descending_key, ic.is_included_column
		FROM sys.indexes i
		JOIN sys.tables t ON t.object_id = i.object_id
		JOIN sys.schemas s ON s.schema_id = t.schema_id
		JOIN sys.index_columns ic
		     ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		JOIN sys.columns col
		     ON col.object_id = ic.object_id AND col.column_id = ic.column_id
		WHERE s.name = @p1 AND i.is_primary_key = 0 AND i.is_unique_constraint = 0
		      AND i.type IN (1, 2) AND i.name IS NOT NULL
		ORDER BY t.name, i.name, ic.is_included_column, ic.key_ordinal`, ns)
	if err != nil {
		return err
	}
	type key struct{ table, name string }
	seen := map[key]*schema.Index{}
	for r.next() {
		t := byName[r.str(0)]
		if t == nil {
			continue
		}
		k := key{r.str(0), r.str(1)}
		ix := seen[k]
		if ix == nil {
			ix = &schema.Index{
				Name:   k.name,
				Unique: r.bool(2),
				// The filter is the server's own rendering, parentheses and
				// all. Peeled for the same reason a default is: they are the
				// catalogue's brackets, not the model's.
				Where: unwrap(r.str(3)),
			}
			seen[k] = ix
			t.Indexes = append(t.Indexes, ix)
		}
		if r.bool(6) {
			ix.Include = append(ix.Include, r.str(4))
			continue
		}
		ix.Columns = append(ix.Columns, schema.IndexColumn{
			Name: r.str(4), Desc: r.bool(5),
		})
	}
	return r.close()
}

func loadForeignKeys(ctx context.Context, c Conn, ns string, s *schema.Schema,
	byName map[string]*schema.Table) error {
	r, err := query(ctx, c, `
		SELECT t.name, fk.name, rt.name, pc.name, rc.name,
		       fk.delete_referential_action, fk.update_referential_action
		FROM sys.foreign_keys fk
		JOIN sys.tables t ON t.object_id = fk.parent_object_id
		JOIN sys.schemas s ON s.schema_id = t.schema_id
		JOIN sys.tables rt ON rt.object_id = fk.referenced_object_id
		JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
		JOIN sys.columns pc
		     ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
		JOIN sys.columns rc
		     ON rc.object_id = fkc.referenced_object_id
		     AND rc.column_id = fkc.referenced_column_id
		WHERE s.name = @p1
		ORDER BY t.name, fk.name, fkc.constraint_column_id`, ns)
	if err != nil {
		return err
	}
	type key struct{ table, name string }
	seen := map[key]*schema.ForeignKey{}
	for r.next() {
		t := byName[r.str(0)]
		if t == nil {
			continue
		}
		k := key{r.str(0), r.str(1)}
		fk := seen[k]
		if fk == nil {
			fk = &schema.ForeignKey{
				Name:     k.name,
				RefTable: r.str(2),
				OnDelete: refAction(r.int(5)),
				OnUpdate: refAction(r.int(6)),
			}
			seen[k] = fk
			t.ForeignKeys = append(t.ForeignKeys, fk)
		}
		fk.Columns = append(fk.Columns, r.str(3))
		fk.RefColumns = append(fk.RefColumns, r.str(4))
	}
	return r.close()
}

// refAction maps the catalogue's numbering onto storm's actions.
//
// NO ACTION comes back as the EMPTY action rather than as "NO ACTION": storm's
// zero value is no action, and importing the words would make every foreign key
// in the generated model carry a clause the author never wrote and that changes
// nothing.
func refAction(n int64) schema.Action {
	switch n {
	case 1:
		return schema.Cascade
	case 2:
		return schema.SetNull
	case 3:
		return "SET DEFAULT"
	}
	return schema.NoAction
}
