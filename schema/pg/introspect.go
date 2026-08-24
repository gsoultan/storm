// Package pg reads a live PostgreSQL database into raorm's schema IR.
//
// It is the front end behind `raorm import` (adopting an existing database) and
// `raorm verify --drift` (catching a production schema that no longer matches
// the model).
package pg

import (
	"context"
	"fmt"

	"github.com/gsoultan/raorm/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Conn is the slice of pgx this package needs.
type Conn interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Introspect reads one namespace (usually "public") into the IR.
func Introspect(ctx context.Context, c Conn, namespace string) (_ *schema.Schema, err error) {
	if namespace == "" {
		namespace = "public"
	}

	// Read with search_path set to the namespace being read.
	//
	// Postgres renders a stored expression relative to search_path:
	// `DEFAULT 'pending'::status` comes back as `'pending'::status` when the
	// enum's schema is on the path and `'pending'::app.status` when it is not.
	// Both describe the same default, and catalog-form-versus-catalog-form
	// comparison — which is the whole reason migrate normalises through a
	// scratch schema — only works if BOTH sides were rendered the same way.
	//
	// Without this, any enum outside `public` makes `raorm verify` report drift
	// forever and `raorm diff` emit a migration that changes nothing. Found by
	// diffing a namespace that was not public.
	if sp, ok := c.(searchPather); ok {
		restore, err := setSearchPath(ctx, sp, namespace)
		if err != nil {
			return nil, err
		}
		defer func() {
			if e := restore(); e != nil && err == nil {
				err = e
			}
		}()
	}

	s := &schema.Schema{}
	if err := loadEnums(ctx, c, namespace, s); err != nil {
		return nil, fmt.Errorf("enums: %w", err)
	}
	if err := loadTables(ctx, c, namespace, s); err != nil {
		return nil, fmt.Errorf("tables: %w", err)
	}
	if err := loadColumns(ctx, c, namespace, s); err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	if err := loadConstraints(ctx, c, namespace, s); err != nil {
		return nil, fmt.Errorf("constraints: %w", err)
	}
	if err := loadIndexes(ctx, c, namespace, s); err != nil {
		return nil, fmt.Errorf("indexes: %w", err)
	}
	s.Normalize()
	return s, nil
}

func loadEnums(ctx context.Context, c Conn, ns string, s *schema.Schema) error {
	rows, err := c.Query(ctx, `
		SELECT t.typname, e.enumlabel
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		JOIN pg_enum e ON e.enumtypid = t.oid
		WHERE n.nspname = $1
		ORDER BY t.typname, e.enumsortorder`, ns)
	if err != nil {
		return err
	}
	defer rows.Close()
	byName := map[string]*schema.Enum{}
	for rows.Next() {
		var name, label string
		if err := rows.Scan(&name, &label); err != nil {
			return err
		}
		e := byName[name]
		if e == nil {
			e = &schema.Enum{Name: name}
			byName[name] = e
			s.Enums = append(s.Enums, e)
		}
		e.Labels = append(e.Labels, label)
	}
	return rows.Err()
}

func loadTables(ctx context.Context, c Conn, ns string, s *schema.Schema) error {
	rows, err := c.Query(ctx, `
		SELECT cl.relname, COALESCE(obj_description(cl.oid, 'pg_class'), '')
		FROM pg_class cl
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		WHERE n.nspname = $1 AND cl.relkind = 'r'
		  AND cl.relname NOT LIKE 'raorm\_%'
		ORDER BY cl.relname`, ns)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		t := &schema.Table{}
		if err := rows.Scan(&t.Name, &t.Comment); err != nil {
			return err
		}
		s.Tables = append(s.Tables, t)
	}
	return rows.Err()
}

func loadColumns(ctx context.Context, c Conn, ns string, s *schema.Schema) error {
	rows, err := c.Query(ctx, `
		SELECT cl.relname, a.attname, a.attnum,
		       t.typname, bt.typname,
		       a.attnotnull,
		       a.atttypmod,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
		       a.attidentity <> '',
		       a.attgenerated <> '',
		       COALESCE(pg_get_expr(gd.adbin, gd.adrelid), ''),
		       (t.typtype = 'e' OR COALESCE(bt.typtype, ' ') = 'e'),
		       COALESCE(col_description(cl.oid, a.attnum), '')
		FROM pg_attribute a
		JOIN pg_class cl ON cl.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		JOIN pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_type bt ON bt.oid = t.typelem
		LEFT JOIN pg_attrdef d ON d.adrelid = cl.oid AND d.adnum = a.attnum AND a.attgenerated = ''
		LEFT JOIN pg_attrdef gd ON gd.adrelid = cl.oid AND gd.adnum = a.attnum AND a.attgenerated <> ''
		WHERE n.nspname = $1 AND cl.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped
		  AND cl.relname NOT LIKE 'raorm\_%'
		ORDER BY cl.relname, a.attnum`, ns)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, name, typ string
		var elemTyp *string
		var attnum, typmod int32
		var notNull, identity, generated, isEnum bool
		var def, genExpr, comment string
		if err := rows.Scan(&tbl, &name, &attnum, &typ, &elemTyp, &notNull,
			&typmod, &def, &identity, &generated, &genExpr, &isEnum, &comment); err != nil {
			return err
		}
		t := s.Table(tbl)
		if t == nil {
			continue
		}
		t.Columns = append(t.Columns, &schema.Column{
			Name:      name,
			Type:      pgType(typ, elemTyp, typmod, isEnum),
			NotNull:   notNull,
			Default:   def,
			Identity:  identity,
			Generated: genExpr,
			Comment:   comment,
		})
	}
	return rows.Err()
}

// pgType maps a pg_type row onto the IR. Array types arrive as "_text" with the
// element type in typelem.
func pgType(name string, elem *string, typmod int32, isEnum bool) schema.Type {
	t := schema.Type{Name: name, Enum: isEnum}
	if len(name) > 1 && name[0] == '_' && elem != nil {
		t.Name = *elem
		t.Array = true
	}
	switch t.Name {
	case "varchar", "bpchar":
		if typmod > 4 {
			t.Size = int(typmod - 4)
		}
	case "numeric":
		if typmod > 4 {
			m := typmod - 4
			t.Precision = int(m >> 16)
			t.Scale = int(m & 0xffff)
		}
	}
	return t
}

func loadConstraints(ctx context.Context, c Conn, ns string, s *schema.Schema) error {
	rows, err := c.Query(ctx, `
		SELECT cl.relname, co.conname, co.contype,
		       pg_get_constraintdef(co.oid),
		       COALESCE(array_agg(a.attname ORDER BY k.ord) FILTER (WHERE a.attname IS NOT NULL), '{}'),
		       COALESCE(rf.relname, ''),
		       co.confdeltype, co.confupdtype, co.condeferrable
		FROM pg_constraint co
		JOIN pg_class cl ON cl.oid = co.conrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		LEFT JOIN pg_class rf ON rf.oid = co.confrelid
		LEFT JOIN LATERAL unnest(co.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		LEFT JOIN pg_attribute a ON a.attrelid = cl.oid AND a.attnum = k.attnum
		WHERE n.nspname = $1 AND cl.relkind = 'r'
		  AND cl.relname NOT LIKE 'raorm\_%'
		GROUP BY cl.relname, co.conname, co.contype, co.oid, rf.relname,
		         co.confdeltype, co.confupdtype, co.condeferrable
		ORDER BY cl.relname, co.conname`, ns)
	if err != nil {
		return err
	}
	defer rows.Close()
	type pendingFK struct {
		t   *schema.Table
		fk  *schema.ForeignKey
		def string
	}
	var fks []pendingFK
	for rows.Next() {
		var tbl, name, def, refTbl string
		var ctype, delAct, updAct string
		var cols []string
		var deferrable bool
		if err := rows.Scan(&tbl, &name, &ctype, &def, &cols, &refTbl,
			&delAct, &updAct, &deferrable); err != nil {
			return err
		}
		t := s.Table(tbl)
		if t == nil {
			continue
		}
		switch ctype {
		case "p":
			t.PrimaryKey = cols
		case "u":
			t.Uniques = append(t.Uniques, &schema.Unique{Name: name, Columns: cols})
		case "c":
			t.Checks = append(t.Checks, &schema.Check{Name: name, Expr: stripCheck(def)})
		case "f":
			fk := &schema.ForeignKey{
				Name: name, Columns: cols, RefTable: refTbl,
				OnDelete: fkAction(delAct), OnUpdate: fkAction(updAct),
				Deferrable: deferrable,
			}
			t.ForeignKeys = append(t.ForeignKeys, fk)
			fks = append(fks, pendingFK{t, fk, def})
		case "x":
			t.Excludes = append(t.Excludes, parseExclude(name, def))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range fks {
		p.fk.RefColumns = refColumns(p.def)
	}
	return nil
}

func fkAction(c string) schema.Action {
	switch c {
	case "r":
		return schema.Restrict
	case "c":
		return schema.Cascade
	case "n":
		return schema.SetNull
	case "d":
		return schema.SetDefault
	default:
		return schema.NoAction
	}
}

func loadIndexes(ctx context.Context, c Conn, ns string, s *schema.Schema) error {
	// Indexes backing a primary key or a unique/exclusion constraint are not
	// separate objects; skip them so the IR does not double-count.
	rows, err := c.Query(ctx, `
		SELECT cl.relname, ic.relname, pg_get_indexdef(i.indexrelid), i.indisunique, am.amname
		FROM pg_index i
		JOIN pg_class cl ON cl.oid = i.indrelid
		JOIN pg_class ic ON ic.oid = i.indexrelid
		JOIN pg_am am ON am.oid = ic.relam
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		WHERE n.nspname = $1 AND cl.relkind = 'r'
		  AND cl.relname NOT LIKE 'raorm\_%'
		  AND NOT EXISTS (SELECT 1 FROM pg_constraint co WHERE co.conindid = i.indexrelid)
		ORDER BY cl.relname, ic.relname`, ns)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, name, def, method string
		var unique bool
		if err := rows.Scan(&tbl, &name, &def, &unique, &method); err != nil {
			return err
		}
		t := s.Table(tbl)
		if t == nil {
			continue
		}
		cols, where := parseIndexDef(def)
		t.Indexes = append(t.Indexes, &schema.Index{
			Name: name, Columns: cols, Unique: unique, Method: method, Where: where,
		})
	}
	return rows.Err()
}

// searchPather is the slice of a connection needed to scope a read. It is an
// interface so Introspect keeps working for a Conn that cannot set it — an
// offline snapshot reader, say — rather than requiring one.
type searchPather interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// setSearchPath scopes the connection to ns and returns a restore function.
//
// The namespace is validated rather than quoted because SET does not take a
// parameter, so it is the one place a name reaches SQL text. Names come from
// configuration, never from a request, and the check costs nothing.
func setSearchPath(ctx context.Context, c searchPather, ns string) (func() error, error) {
	if err := validNamespace(ns); err != nil {
		return nil, err
	}
	var prev string
	if err := c.QueryRow(ctx, "SHOW search_path").Scan(&prev); err != nil {
		return nil, fmt.Errorf("read search_path: %w", err)
	}
	if _, err := c.Exec(ctx, "SET search_path TO "+ns); err != nil {
		return nil, fmt.Errorf("set search_path to %s: %w", ns, err)
	}
	return func() error {
		if _, err := c.Exec(ctx, "SET search_path TO "+prev); err != nil {
			return fmt.Errorf("restore search_path: %w", err)
		}
		return nil
	}, nil
}

func validNamespace(s string) error {
	if s == "" || len(s) > 63 {
		return fmt.Errorf("invalid schema name %q", s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c == '_' || (i > 0 && (c >= '0' && c <= '9'))
		if !ok {
			return fmt.Errorf("invalid schema name %q: only lowercase letters, digits and underscore", s)
		}
	}
	return nil
}
