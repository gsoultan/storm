// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package schemasqlite reads a SQLite database into storm's IR.
//
// The on-ramp, as for every other target: `storm import` prints the model a
// database implies, and `storm diff` and migrate compare a model against one.
//
// SQLITE HAS NO CATALOGUE in the sense the others do. What it keeps is the
// CREATE statement each table and index was made with, verbatim, in
// sqlite_schema, and a set of pragmas that answer structural questions about
// it. This package reads both: the pragmas for everything they answer —
// columns, nullability, defaults, keys, index columns, foreign key columns and
// actions — and the statement for the rest, which is every NAME a constraint
// was given, every CHECK's text, every generated column's expression, a
// foreign key's DEFERRABLE, and an index's expression keys and predicate. See
// parse.go.
//
// THE TYPE IS WHAT IT WAS DECLARED AS. SQLite stores any value in any column,
// and the declared type is a string it keeps and consults only for affinity;
// so the inverse of compile/liteddl's mapping is exact for a database storm
// made, and an affinity guess for one it did not. Two of liteddl's constraints
// come back as the type they encode rather than as checks: a VARCHAR(n)'s
// length check, and a document column's json_valid. Without that, every
// varchar a model declares would diff as a varchar plus a check nobody wrote.
//
// It reads the value side of the port, as schema/oracle does: the adapter is
// over database/sql, so the rows are decoded values rather than wire bytes.
package schemasqlite

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/v2/runtime"
	"github.com/gsoultan/storm/v2/runtime/valdec"
	"github.com/gsoultan/storm/v2/schema"
)

// Conn is the slice of a driver this package needs. runtime/sqlitedrv's Exec
// and Pool satisfy it.
type Conn interface {
	Query(ctx context.Context, sql string, args []any) (runtime.Rows, error)
}

// now is compile/liteddl.Now, the clock a now() default is stored as. Spelled
// out rather than imported: schema/ does not depend on compile/, and a test
// holds the two together.
const now = "strftime('%Y-%m-%dT%H:%M:%f000000Z', 'now')"

// Introspect reads one database into the IR. ns is the attached database's
// name — "main" when empty, which is the file the connection opened.
func Introspect(ctx context.Context, c Conn, ns string) (*schema.Schema, error) {
	if ns == "" {
		ns = "main"
	}
	s := &schema.Schema{}
	names, sqls, err := tables(ctx, c, ns)
	if err != nil {
		return nil, fmt.Errorf("tables: %w", err)
	}
	for i, name := range names {
		t, err := table(ctx, c, ns, name, sqls[i])
		if err != nil {
			return nil, fmt.Errorf("table %s: %w", name, err)
		}
		s.Tables = append(s.Tables, t)
	}
	resolveRefColumns(s)
	s.Normalize()
	return s, nil
}

func tables(ctx context.Context, c Conn, ns string) ([]string, []string, error) {
	r, err := query(ctx, c, `SELECT name, sql FROM `+ident(ns)+`.sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, nil, err
	}
	defer r.close()
	var names, sqls []string
	for r.next() {
		names = append(names, r.str(0))
		sqls = append(sqls, r.str(1))
	}
	return names, sqls, r.close()
}

func table(ctx context.Context, c Conn, ns, name, sql string) (*schema.Table, error) {
	t := &schema.Table{Name: name}
	def := parseTable(sql)
	if err := columns(ctx, c, ns, t, def); err != nil {
		return nil, err
	}
	checks(t, def)
	if err := indexes(ctx, c, ns, t, def); err != nil {
		return nil, err
	}
	if err := foreignKeys(ctx, c, ns, t, def); err != nil {
		return nil, err
	}
	return t, nil
}

func columns(ctx context.Context, c Conn, ns string, t *schema.Table, def tableDef) error {
	r, err := query(ctx, c, `SELECT name, type, "notnull", dflt_value, pk, hidden
		FROM pragma_table_xinfo(?1, ?2) ORDER BY cid`, t.Name, ns)
	if err != nil {
		return err
	}
	defer r.close()
	type keyed struct {
		name string
		pos  int64
	}
	var pk []keyed
	for r.next() {
		hidden := r.int(5)
		if hidden == 1 {
			continue // a virtual table's hidden column, not one a model declares
		}
		col := &schema.Column{
			Name:    r.str(0),
			Type:    typeOf(r.str(1)),
			NotNull: r.int(2) != 0,
		}
		if hidden == 2 || hidden == 3 {
			col.Generated = def.generated[col.Name]
		} else if !r.isNull(3) {
			col.Default = defaultOf(r.str(3))
		}
		if p := r.int(4); p > 0 {
			pk = append(pk, keyed{col.Name, p})
		}
		t.Columns = append(t.Columns, col)
	}
	if err := r.close(); err != nil {
		return err
	}
	for i := 1; i <= len(pk); i++ {
		for _, k := range pk {
			if k.pos == int64(i) {
				t.PrimaryKey = append(t.PrimaryKey, k.name)
			}
		}
	}
	// THE ROWID. A sole INTEGER key with AUTOINCREMENT is the only key the
	// server generates here, and it is what compile/liteddl renders every
	// identity and serial as — read back as the int8 identity it holds.
	if len(t.PrimaryKey) == 1 && def.autoincrement {
		if col := t.Column(t.PrimaryKey[0]); col != nil && col.Type.Name == schema.TypeInt4 {
			col.Type = schema.Type{Name: schema.TypeInt8}
			col.Identity = true
			col.NotNull = true
		}
	}
	return nil
}

// typeOf maps a declared type back to storm's: exactly for the names
// compile/liteddl writes, and by SQLite's own affinity rules for the rest.
func typeOf(declared string) schema.Type {
	d := strings.ToUpper(strings.TrimSpace(declared))
	base, args := d, ""
	if i := strings.IndexByte(d, '('); i >= 0 {
		base = strings.TrimSpace(d[:i])
		args = strings.TrimSuffix(strings.TrimSpace(d[i+1:]), ")")
	}
	nums := func() (int, int) {
		parts := strings.Split(args, ",")
		p, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		s := 0
		if len(parts) > 1 {
			s, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
		}
		return p, s
	}
	switch base {
	case "BOOLEAN", "BOOL":
		return schema.Type{Name: schema.TypeBool}
	case "SMALLINT", "INT2":
		return schema.Type{Name: schema.TypeInt2}
	case "INTEGER", "INT", "INT4", "MEDIUMINT":
		return schema.Type{Name: schema.TypeInt4}
	case "BIGINT", "INT8":
		return schema.Type{Name: schema.TypeInt8}
	case "REAL", "FLOAT":
		return schema.Type{Name: schema.TypeFloat4}
	case "DOUBLE", "DOUBLE PRECISION":
		return schema.Type{Name: schema.TypeFloat8}
	case "NUMERIC", "DECIMAL":
		p, s := nums()
		return schema.Type{Name: schema.TypeNumeric, Precision: p, Scale: s}
	case "VARCHAR", "CHARACTER VARYING", "NVARCHAR", "VARYING CHARACTER":
		n, _ := nums()
		if n == 0 {
			return schema.Type{Name: schema.TypeText}
		}
		return schema.Type{Name: schema.TypeVarchar, Size: n}
	case "TEXT", "CLOB", "CHAR", "CHARACTER", "NCHAR", "NATIVE CHARACTER":
		return schema.Type{Name: schema.TypeText}
	case "BLOB", "":
		return schema.Type{Name: schema.TypeBytea}
	case "UUID":
		return schema.Type{Name: schema.TypeUUID}
	case "TIMESTAMP":
		return schema.Type{Name: schema.TypeTimestamptz}
	case "DATETIME":
		return schema.Type{Name: schema.TypeTimestamp}
	case "DATE":
		return schema.Type{Name: schema.TypeDate}
	case "TIME":
		return schema.Type{Name: schema.TypeTime}
	case "JSON", "JSONB":
		return schema.Type{Name: schema.TypeJSONB}
	}
	// SQLite's affinity rules, in its own order: INT, then text, then blob,
	// then real, and NUMERIC for anything else.
	switch {
	case strings.Contains(base, "INT"):
		return schema.Type{Name: schema.TypeInt8}
	case strings.Contains(base, "CHAR"), strings.Contains(base, "CLOB"), strings.Contains(base, "TEXT"):
		return schema.Type{Name: schema.TypeText}
	case strings.Contains(base, "BLOB"):
		return schema.Type{Name: schema.TypeBytea}
	case strings.Contains(base, "REAL"), strings.Contains(base, "FLOA"), strings.Contains(base, "DOUB"):
		return schema.Type{Name: schema.TypeFloat8}
	}
	return schema.Type{Name: strings.ToLower(declared)}
}

// defaultOf is a stored default as a model writes it: the clock storm stores
// as now(), and the parentheses compile/liteddl puts around an expression
// removed, so the model's `lower('X')` compares equal to the stored one.
func defaultOf(d string) string {
	d = strings.TrimSpace(d)
	for len(d) > 1 && d[0] == '(' && matchParen(d, 0) == len(d)-1 {
		d = strings.TrimSpace(d[1 : len(d)-1])
	}
	if d == now {
		return "now()"
	}
	return d
}

// checks sorts the CHECKs the statement declared into the ones that encode a
// TYPE — read back as that type — and the ones a model declared.
func checks(t *schema.Table, def tableDef) {
	n := 0
	for _, ck := range def.checks {
		if ck.column != "" && ck.name == "" {
			if col := t.Column(ck.column); col != nil && typeCheck(col, ck.expr) {
				continue
			}
		}
		name := ck.name
		if name == "" {
			n++
			name = "ck_" + t.Name + "_" + strconv.Itoa(n)
		}
		t.Checks = append(t.Checks, &schema.Check{Name: name, Expr: ck.expr})
	}
}

// typeCheck reports whether a column's own check is one compile/liteddl writes
// for its type, and makes the column that type if so.
func typeCheck(col *schema.Column, expr string) bool {
	q := ident(col.Name)
	switch {
	case col.Type.Name == schema.TypeVarchar && expr == "length("+q+") <= "+strconv.Itoa(col.Type.Size):
		return true
	case col.Type.Name == schema.TypeText && expr == "json_valid("+q+")":
		col.Type = schema.Type{Name: schema.TypeJSONB}
		return true
	}
	return false
}

func indexes(ctx context.Context, c Conn, ns string, t *schema.Table, def tableDef) error {
	r, err := query(ctx, c, `SELECT l.name, l."unique", l.origin, l.partial, s.sql
		FROM pragma_index_list(?1, ?2) AS l
		LEFT JOIN `+ident(ns)+`.sqlite_schema AS s ON s.type = 'index' AND s.name = l.name
		ORDER BY l.name`, t.Name, ns)
	if err != nil {
		return err
	}
	type listed struct {
		name, origin, sql string
		unique            bool
	}
	var all []listed
	for r.next() {
		all = append(all, listed{name: r.str(0), unique: r.int(1) != 0, origin: r.str(2), sql: r.str(4)})
	}
	if err := r.close(); err != nil {
		return err
	}
	for _, l := range all {
		keys, err := indexKeys(ctx, c, ns, l.name)
		if err != nil {
			return err
		}
		switch l.origin {
		case "pk":
			// The table's key, which columns() already read.
		case "u":
			cols := make([]string, len(keys))
			for i, k := range keys {
				cols[i] = k.Name
			}
			name := ""
			for _, u := range def.uniques {
				if sameNames(u.cols, cols) {
					name = u.name
				}
			}
			if name == "" {
				name = "uq_" + t.Name + "_" + strings.Join(cols, "_")
			}
			t.Uniques = append(t.Uniques, &schema.Unique{Name: name, Columns: cols})
		default:
			exprs, where := parseIndex(l.sql)
			for i := range keys {
				if keys[i].Expr && i < len(exprs) {
					keys[i].Name = exprs[i]
				}
			}
			t.Indexes = append(t.Indexes, &schema.Index{
				Name: l.name, Unique: l.unique, Columns: keys, Where: where,
			})
		}
	}
	return nil
}

// indexKeys reads an index's key columns, in order. An expression key comes
// back unnamed, and its text is the caller's to fill from the statement.
func indexKeys(ctx context.Context, c Conn, ns, index string) ([]schema.IndexColumn, error) {
	r, err := query(ctx, c, `SELECT cid, name, "desc", coll FROM pragma_index_xinfo(?1, ?2)
		WHERE "key" = 1 ORDER BY seqno`, index, ns)
	if err != nil {
		return nil, err
	}
	defer r.close()
	var out []schema.IndexColumn
	for r.next() {
		k := schema.IndexColumn{Name: r.str(1), Desc: r.int(2) != 0}
		if r.int(0) == -2 {
			k.Expr = true
		}
		if coll := r.str(3); coll != "" && !strings.EqualFold(coll, "BINARY") {
			k.Collate = coll
		}
		out = append(out, k)
	}
	return out, r.close()
}

func foreignKeys(ctx context.Context, c Conn, ns string, t *schema.Table, def tableDef) error {
	r, err := query(ctx, c, `SELECT id, "table", "from", "to", on_update, on_delete
		FROM pragma_foreign_key_list(?1, ?2) ORDER BY id, seq`, t.Name, ns)
	if err != nil {
		return err
	}
	defer r.close()
	byID := map[int64]*schema.ForeignKey{}
	for r.next() {
		id := r.int(0)
		fk := byID[id]
		if fk == nil {
			fk = &schema.ForeignKey{RefTable: r.str(1), OnUpdate: actionOf(r.str(4)), OnDelete: actionOf(r.str(5))}
			byID[id] = fk
			t.ForeignKeys = append(t.ForeignKeys, fk)
		}
		fk.Columns = append(fk.Columns, r.str(2))
		// NULL when the key names only the table, and so its primary key —
		// resolved once every table has been read.
		fk.RefColumns = append(fk.RefColumns, r.str(3))
	}
	if err := r.close(); err != nil {
		return err
	}
	for _, fk := range t.ForeignKeys {
		for _, p := range def.fks {
			if sameNames(p.cols, fk.Columns) {
				fk.Name, fk.Deferrable = p.name, p.deferrable
			}
		}
		if fk.Name == "" {
			fk.Name = "fk_" + t.Name + "_" + strings.Join(fk.Columns, "_")
		}
	}
	return nil
}

// resolveRefColumns fills a foreign key that named only its parent table with
// the parent's primary key, which is what it references.
func resolveRefColumns(s *schema.Schema) {
	for _, t := range s.Tables {
		for _, fk := range t.ForeignKeys {
			parent := s.Table(fk.RefTable)
			if parent == nil {
				continue
			}
			for i, rc := range fk.RefColumns {
				if rc == "" && i < len(parent.PrimaryKey) {
					fk.RefColumns[i] = parent.PrimaryKey[i]
				}
			}
		}
	}
}

func actionOf(s string) schema.Action {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "CASCADE":
		return schema.Cascade
	case "SET NULL":
		return schema.SetNull
	case "SET DEFAULT":
		return schema.SetDefault
	case "RESTRICT":
		return schema.Restrict
	}
	return schema.NoAction
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// ident quotes a name, as compile/liteddl.Ident does.
func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

type rowReader struct {
	rows runtime.ValueRows
	v    []any
}

func query(ctx context.Context, c Conn, sql string, args ...any) (*rowReader, error) {
	rows, err := runtime.AsValueRows(c.Query(ctx, sql, args))
	if err != nil {
		return nil, err
	}
	return &rowReader{rows: rows}, nil
}

func (r *rowReader) next() bool {
	if !r.rows.Next() {
		return false
	}
	r.v = r.rows.Values()
	return true
}

func (r *rowReader) close() error { r.rows.Close(); return r.rows.Err() }

func (r *rowReader) str(i int) string {
	if i >= len(r.v) {
		return ""
	}
	return valdec.Str(r.v[i])
}

func (r *rowReader) int(i int) int64 {
	if i >= len(r.v) {
		return 0
	}
	return valdec.Int8(r.v[i])
}

func (r *rowReader) isNull(i int) bool { return i >= len(r.v) || r.v[i] == nil }
