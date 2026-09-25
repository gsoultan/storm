package schemaoracle

import (
	"context"
	"strings"

	"github.com/gsoultan/storm/schema"
)

func loadTables(ctx context.Context, c Conn, ns string, s *schema.Schema,
	byName map[string]*schema.Table) error {

	pred, arg := owner(ns)
	sql := `SELECT table_name FROM all_tables WHERE ` + pred +
		` AND nested = 'NO' AND secondary = 'N' ORDER BY table_name`
	r, err := queryOwner(ctx, c, sql, arg)
	if err != nil {
		return err
	}
	defer r.close()
	for r.next() {
		t := &schema.Table{Name: r.str(0)}
		s.Tables = append(s.Tables, t)
		byName[t.Name] = t
	}
	return r.close()
}

// queryOwner runs a catalogue query whose only bind is the schema, or none.
func queryOwner(ctx context.Context, c Conn, sql, arg string) (*rowReader, error) {
	if arg == "" {
		return query(ctx, c, sql)
	}
	return query(ctx, c, sql, arg)
}

func loadColumns(ctx context.Context, c Conn, ns string, s *schema.Schema,
	byName map[string]*schema.Table) error {

	pred, arg := owner(ns)
	// data_default is a LONG in the catalogue, which a driver cannot always
	// read — TO_LOB in a subquery is the portable way to get it as text.
	sql := `SELECT table_name, column_name, data_type, data_length, char_length,
		data_precision, data_scale, nullable, data_default, virtual_column, identity_column
		FROM all_tab_cols WHERE ` + pred + ` AND hidden_column = 'NO'
		ORDER BY table_name, column_id`
	r, err := queryOwner(ctx, c, sql, arg)
	if err != nil {
		return err
	}
	defer r.close()
	for r.next() {
		t := byName[r.str(0)]
		if t == nil {
			continue
		}
		col := &schema.Column{
			Name:    r.str(1),
			Type:    typeOf(r.str2(2), r.int(3), r.int(4), r.int(5), r.int(6), r.isNull(5)),
			NotNull: r.str2(7) == "N",
		}
		// A VIRTUAL column's "default" IS its expression — Oracle stores the
		// two in one place — so reading it as a default would produce a model
		// that inserts the expression as a literal.
		if r.str2(9) == "YES" {
			col.Generated = strings.TrimSpace(r.str2(8))
		} else if d := unwrap(strings.TrimSpace(r.str2(8))); !isNoDefault(d) {
			col.Default = d
		}
		if r.str2(10) == "YES" {
			col.Identity = true
			// An identity column's default is the sequence Oracle made for it,
			// which is not a default the model declared.
			col.Default = ""
		}
		t.Columns = append(t.Columns, col)
	}
	return r.close()
}

// typeOf maps an Oracle type back to storm's.
//
// The inverse of compile/oraddl's map, and it has to be: a model imported from
// a database and then applied to one must produce the same columns, or
// `storm diff` reports drift that is not there. The asymmetries are named
// where they occur.
func typeOf(name string, dataLen, charLen, prec, scale int64, precNull bool) schema.Type {
	switch strings.ToUpper(name) {
	case "NUMBER":
		switch {
		case precNull:
			// A bare NUMBER: variable precision, which is what oraddl emits
			// for an unbounded numeric.
			return schema.Type{Name: schema.TypeNumeric}
		case scale > 0:
			return schema.Type{Name: schema.TypeNumeric, Precision: int(prec), Scale: int(scale)}
		case prec <= 5:
			return schema.Type{Name: schema.TypeInt2}
		case prec <= 10:
			return schema.Type{Name: schema.TypeInt4}
		case prec <= 19:
			return schema.Type{Name: schema.TypeInt8}
		default:
			return schema.Type{Name: schema.TypeNumeric, Precision: int(prec)}
		}
	case "BOOLEAN":
		return schema.Type{Name: schema.TypeBool}
	case "BINARY_FLOAT":
		return schema.Type{Name: schema.TypeFloat4}
	case "BINARY_DOUBLE":
		return schema.Type{Name: schema.TypeFloat8}
	case "VARCHAR2", "NVARCHAR2", "CHAR", "NCHAR":
		// char_length, not data_length. data_length is BYTES, and a column
		// declared VARCHAR2(200 CHAR) in a multi-byte character set reports
		// 800 there — which would halve or quadruple every imported width
		// depending on which one a reader picked. M10 shipped exactly that
		// defect against SQL Server's max_length.
		n := charLen
		if n == 0 {
			n = dataLen
		}
		return schema.Type{Name: schema.TypeVarchar, Size: int(n)}
	case "CLOB", "NCLOB", "LONG":
		return schema.Type{Name: schema.TypeText}
	case "BLOB", "RAW", "LONG RAW":
		// RAW(16) is storm's uuid, and nothing else is. A RAW of any other
		// width is bytes — importing it as a uuid would give the model a
		// [16]byte field for a column that is not one.
		if strings.ToUpper(name) == "RAW" && dataLen == 16 {
			return schema.Type{Name: schema.TypeUUID}
		}
		return schema.Type{Name: schema.TypeBytea}
	case "JSON":
		return schema.Type{Name: schema.TypeJSONB}
	case "DATE":
		return schema.Type{Name: schema.TypeDate}
	}
	if strings.HasPrefix(strings.ToUpper(name), "TIMESTAMP") {
		if strings.Contains(strings.ToUpper(name), "TIME ZONE") {
			return schema.Type{Name: schema.TypeTimestamptz}
		}
		return schema.Type{Name: schema.TypeTimestamp}
	}
	// Unknown: kept as its own name rather than guessed at. A model carrying
	// a type storm does not know refuses at generate time with the name in
	// the message, which is better than a silent substitution.
	return schema.Type{Name: strings.ToLower(name)}
}

// isNoDefault reports whether a stored default means "none".
//
// TWO SPELLINGS FOR ONE FACT. A column that never had a default reports
// data_default as SQL NULL, which arrives here as "". A column whose default
// was DROPPED — `MODIFY (c DEFAULT NULL)`, which is how Oracle drops one —
// reports the four characters N-U-L-L. They mean the same thing, and reading
// the second as a default value makes `storm diff` propose to drop a default
// that is already gone, forever.
//
// Found by the migrate gate: the first plan applied, and the second one
// contained the same DEFAULT NULL.
func isNoDefault(d string) bool {
	return d == "" || strings.EqualFold(d, "NULL")
}

// unwrap strips the parentheses Oracle adds around a stored default, so a
// model's `1` compares equal to a catalogue's `(1)`.
func unwrap(s string) string {
	for len(s) > 1 && s[0] == '(' && s[len(s)-1] == ')' && balanced(s) {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

func balanced(s string) bool {
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

	pred, arg := owner(ns)
	sql := `SELECT c.table_name, c.constraint_name, c.constraint_type, cc.column_name
		FROM all_constraints c
		JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name
		WHERE c.` + pred + ` AND c.constraint_type IN ('P','U')
		ORDER BY c.table_name, c.constraint_name, cc.position`
	r, err := queryOwner(ctx, c, sql, arg)
	if err != nil {
		return err
	}
	defer r.close()

	uniques := map[string]*schema.Unique{}
	for r.next() {
		t := byName[r.str(0)]
		if t == nil {
			continue
		}
		name, kind, col := r.str(1), r.str2(2), r.str(3)
		if kind == "P" {
			t.PrimaryKey = append(t.PrimaryKey, col)
			continue
		}
		key := t.Name + "." + name
		u := uniques[key]
		if u == nil {
			u = &schema.Unique{Name: name}
			uniques[key] = u
			t.Uniques = append(t.Uniques, u)
		}
		u.Columns = append(u.Columns, col)
	}
	return r.close()
}

func loadChecks(ctx context.Context, c Conn, ns string, s *schema.Schema,
	byName map[string]*schema.Table) error {

	pred, arg := owner(ns)
	// generated = 'USER NAME' excludes the NOT NULL checks Oracle invents for
	// every such column: it implements NOT NULL as a check constraint with a
	// system-generated name, and importing those would give every model a
	// duplicate of what the column already says.
	sql := `SELECT table_name, constraint_name, search_condition_vc
		FROM all_constraints
		WHERE ` + pred + ` AND constraint_type = 'C' AND generated = 'USER NAME'
		ORDER BY table_name, constraint_name`
	r, err := queryOwner(ctx, c, sql, arg)
	if err != nil {
		return err
	}
	defer r.close()
	for r.next() {
		t := byName[r.str(0)]
		if t == nil {
			continue
		}
		expr := strings.TrimSpace(r.str2(2))
		if expr == "" {
			continue
		}
		t.Checks = append(t.Checks, &schema.Check{Name: r.str(1), Expr: expr})
	}
	return r.close()
}

func loadIndexes(ctx context.Context, c Conn, ns string, s *schema.Schema,
	byName map[string]*schema.Table) error {

	pred, arg := owner(ns)
	// Indexes that BACK a constraint are excluded: a primary key's index and a
	// unique constraint's are the constraint, and importing both would make
	// every diff propose to drop one.
	sql := `SELECT i.table_name, i.index_name, i.uniqueness, ic.column_name, ic.descend,
		ie.column_expression
		FROM all_indexes i
		JOIN all_ind_columns ic ON ic.index_owner = i.owner AND ic.index_name = i.index_name
		LEFT JOIN all_ind_expressions ie ON ie.index_owner = i.owner
			AND ie.index_name = i.index_name AND ie.column_position = ic.column_position
		WHERE i.` + pred + ` AND i.index_type IN ('NORMAL','FUNCTION-BASED NORMAL')
		AND NOT EXISTS (SELECT 1 FROM all_constraints c
			WHERE c.owner = i.owner AND c.index_name = i.index_name)
		ORDER BY i.table_name, i.index_name, ic.column_position`
	r, err := queryOwner(ctx, c, sql, arg)
	if err != nil {
		return err
	}
	defer r.close()

	seen := map[string]*schema.Index{}
	for r.next() {
		t := byName[r.str(0)]
		if t == nil {
			continue
		}
		name := r.str(1)
		key := t.Name + "." + name
		ix := seen[key]
		if ix == nil {
			ix = &schema.Index{Name: name, Unique: r.str2(2) == "UNIQUE", Method: "btree"}
			seen[key] = ix
			t.Indexes = append(t.Indexes, ix)
		}
		k := schema.IndexColumn{Name: r.str(3), Desc: r.str2(4) == "DESC"}
		if e := strings.TrimSpace(r.str2(5)); e != "" {
			// A function-based index: the key is an EXPRESSION, and its text
			// is the model's own SQL rather than a name — so it is not folded.
			//
			// This is where a partial UNIQUE comes back: oraddl writes one as
			// `CASE WHEN <pred> THEN col END`, and the catalogue hands that
			// text straight back.
			k.Name = e
			k.Expr = true
		}
		ix.Columns = append(ix.Columns, k)
	}
	return r.close()
}

func loadForeignKeys(ctx context.Context, c Conn, ns string, s *schema.Schema,
	byName map[string]*schema.Table) error {

	pred, arg := owner(ns)
	sql := `SELECT c.table_name, c.constraint_name, cc.column_name,
		rc.table_name, rcc.column_name, c.delete_rule
		FROM all_constraints c
		JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name
		JOIN all_constraints rc ON rc.owner = c.r_owner AND rc.constraint_name = c.r_constraint_name
		JOIN all_cons_columns rcc ON rcc.owner = rc.owner
			AND rcc.constraint_name = rc.constraint_name AND rcc.position = cc.position
		WHERE c.` + pred + ` AND c.constraint_type = 'R'
		ORDER BY c.table_name, c.constraint_name, cc.position`
	r, err := queryOwner(ctx, c, sql, arg)
	if err != nil {
		return err
	}
	defer r.close()

	seen := map[string]*schema.ForeignKey{}
	for r.next() {
		t := byName[r.str(0)]
		if t == nil {
			continue
		}
		name := r.str(1)
		key := t.Name + "." + name
		fk := seen[key]
		if fk == nil {
			fk = &schema.ForeignKey{
				Name:     name,
				RefTable: r.str(3),
				OnDelete: deleteRule(r.str2(5)),
				// NO ON UPDATE, ever. Oracle has no such clause on a foreign
				// key — compile/oraddl refuses a model that asks for one — so
				// there is nothing in the catalogue to read.
			}
			seen[key] = fk
			t.ForeignKeys = append(t.ForeignKeys, fk)
		}
		fk.Columns = append(fk.Columns, r.str(2))
		fk.RefColumns = append(fk.RefColumns, r.str(4))
	}
	return r.close()
}

func deleteRule(s string) schema.Action {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "CASCADE":
		return schema.Cascade
	case "SET NULL":
		return schema.SetNull
	}
	// NO ACTION is Oracle's default and is spelled by saying nothing, which is
	// what schema.NoAction is.
	return schema.NoAction
}
