// Package oraddl renders a storm schema as Oracle DDL, or says why it cannot.
//
// Written before runtime/oradrv, on purpose and for the third time: the SQL is
// the half that can be proven with somebody else's client, and proving it after
// writing a driver against untested SQL is the order M9 showed is expensive.
// internal/oraclespike carries this text to a real server.
//
// Oracle 23 is the target, and that decision does work. Native BOOLEAN (23c),
// native JSON (21c), IDENTITY columns (12c) and 128-character identifiers
// (12.2) all exist there, and each was measured rather than assumed —
// internal/oraclespike/README.md, result 1. A back end written for 19c would
// need NUMBER(1) plus a CHECK for every bool, a CLOB plus IS JSON for every
// document, and a sequence object per key.
//
// THE THING THIS PACKAGE EXISTS TO REFUSE is not a type. It is Oracle's empty
// string, which is NULL — and Check's rule for it is the whole reason M12 was
// not cancelled. See checkEmptyString.
package oraddl

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Statements renders a schema as the individual statements Oracle's PROTOCOL
// takes, or reports every reason it cannot.
//
// One per call, and with NO trailing semicolon. Through TTC a trailing `;` is
// ORA-00911 "invalid character" — unlike every other target storm has, where it
// is either required or harmless. That is why this is the primitive and Create
// is the thing built from it, rather than the other way round: the protocol is
// the harder constraint, and a splitter that has to strip a terminator storm
// itself added is a splitter that can be wrong.
func Statements(s *schema.Schema) ([]string, error) {
	if err := Check(s); err != nil {
		return nil, err
	}
	enums := enumsOf(s)
	var out []string
	for _, t := range s.Tables {
		def, err := CreateTable(t, enums)
		if err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	// Indexes after every table, and foreign keys after every index: a key
	// pointing at a table that does not exist yet is a migration that fails
	// half way through a deployment.
	for _, t := range s.Tables {
		for _, ix := range t.Indexes {
			out = append(out, CreateIndex(t, ix))
		}
	}
	for _, t := range s.Tables {
		for _, fk := range t.ForeignKeys {
			out = append(out, AddForeignKey(t, fk))
		}
	}
	return out, nil
}

// Create renders a schema as a migration FILE, which is a different medium
// with a different rule: SQL*Plus and every Oracle migration runner split on
// the semicolon this adds and the protocol refuses.
func Create(s *schema.Schema) (string, error) {
	stmts, err := Statements(s)
	if err != nil {
		return "", err
	}
	return strings.Join(stmts, ";\n\n") + ";\n", nil
}

// enumsOf indexes a schema's enums by name, which is what a column carrying one
// needs and a table on its own has no way to reach.
func enumsOf(s *schema.Schema) map[string]*schema.Enum {
	m := make(map[string]*schema.Enum, len(s.Enums))
	for _, e := range s.Enums {
		m[e.Name] = e
	}
	return m
}

// CreateTable renders one table and its table-level constraints.
func CreateTable(t *schema.Table, enums map[string]*schema.Enum) (string, error) {
	var b strings.Builder
	b.WriteString("CREATE TABLE " + Ident(t.Name) + " (\n")
	parts := make([]string, 0, len(t.Columns)+2)
	for _, c := range t.Columns {
		def, err := ColumnDef(t.Name, c, enums)
		if err != nil {
			return "", err
		}
		parts = append(parts, "    "+def)
	}
	// An enum's CHECK is a TABLE-level constraint, the same cut msddl made and
	// for the same reason: Oracle has no enum type either, and the set of
	// accepted values is the whole of what the declaration means.
	for _, c := range t.Columns {
		if !c.Type.Enum {
			continue
		}
		if e := enums[c.Type.Name]; e != nil {
			parts = append(parts, "    CONSTRAINT "+Ident(enumCheckName(t.Name, c.Name))+
				" CHECK ("+EnumCheck(c.Name, e)+")")
		}
	}
	if len(t.PrimaryKey) > 0 {
		parts = append(parts, "    PRIMARY KEY ("+identList(t.PrimaryKey)+")")
	}
	for _, u := range t.Uniques {
		parts = append(parts, "    CONSTRAINT "+Ident(u.Name)+" UNIQUE ("+identList(u.Columns)+")")
	}
	for _, ck := range t.Checks {
		parts = append(parts, "    CONSTRAINT "+Ident(ck.Name)+" CHECK ("+checkExpr(ck)+")")
	}
	b.WriteString(strings.Join(parts, ",\n"))
	b.WriteString("\n)")
	return b.String(), nil
}

// enumCheckName is what an enum column's constraint is called. Derived rather
// than declared, exactly as msddl derives it: the model has nowhere to put the
// name, and a diff that re-renders has to arrive at the same one twice.
func enumCheckName(table, col string) string { return "ck_" + table + "_" + col }

// ColumnDef renders one column.
func ColumnDef(table string, c *schema.Column, enums map[string]*schema.Enum) (string, error) {
	if c.Generated != "" {
		// A VIRTUAL column, which is Oracle's only generated form: there is no
		// STORED equivalent. It is computed on read and CAN be indexed, which
		// is what storm needs it for, but it occupies no space and a query that
		// selects it pays the expression every time.
		//
		// The type is omitted, as it is on SQL Server: Oracle derives it.
		def := Ident(c.Name) + " GENERATED ALWAYS AS (" + c.Generated + ") VIRTUAL"
		if c.NotNull {
			def += " NOT NULL"
		}
		return def, nil
	}
	ty, err := typeOf(table, c, enums)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(Ident(c.Name) + " " + ty)
	switch {
	case c.Identity || c.Serial:
		// GENERATED BY DEFAULT rather than ALWAYS: storm's write path may
		// supply the key — a caller who set it, or a plan that carries one —
		// and ALWAYS refuses that with ORA-32795 rather than accepting it.
		// Serial and Identity collapse here because Oracle has only this form;
		// the sequence it makes is an object, but not one the model names.
		b.WriteString(" GENERATED BY DEFAULT AS IDENTITY")
	case c.Default != "":
		if d := oracleDefault(c); d != "" {
			b.WriteString(" DEFAULT " + d)
		}
	}
	if c.NotNull {
		b.WriteString(" NOT NULL")
	}
	return b.String(), nil
}

// TypeEnum is the column type an enum lowers to: the widest label, as
// characters.
//
// Same shape as msddl's, and the width is in CHARacters rather than bytes for
// the reason TypeSQL gives below — a label with an accent in it is two bytes
// and one character, and a byte-counted column would refuse it.
func TypeEnum(e *schema.Enum) string {
	w := 1
	for _, l := range e.Labels {
		if n := len([]rune(l)); n > w {
			w = n
		}
	}
	return fmt.Sprintf("VARCHAR2(%d CHAR)", w)
}

// EnumCheck is the constraint that makes a string column an enum.
func EnumCheck(col string, e *schema.Enum) string {
	vals := make([]string, len(e.Labels))
	for i, l := range e.Labels {
		vals[i] = quoteLit(l)
	}
	return Ident(col) + " IN (" + strings.Join(vals, ", ") + ")"
}

// typeOf is TypeSQL with the enum labels to hand.
func typeOf(table string, c *schema.Column, enums map[string]*schema.Enum) (string, error) {
	if c.Type.Enum {
		e := enums[c.Type.Name]
		if e == nil {
			return "", fmt.Errorf(
				"oraddl: %s.%s is an enum and %s is not declared in the schema",
				table, c.Name, c.Type.Name)
		}
		return TypeEnum(e), nil
	}
	return TypeSQL(table, c)
}

// ColumnType is the type Oracle will store a column AS, with the enum labels to
// hand. migrate needs it: an ALTER COLUMN restates the type.
func ColumnType(table string, c *schema.Column, enums map[string]*schema.Enum) (string, error) {
	return typeOf(table, c, enums)
}

// maxVarchar2 is the default MAX_STRING_SIZE=STANDARD limit, in bytes. EXTENDED
// raises it to 32767, but that is a database-level setting a library cannot
// assume — the same reasoning msddl applies to QUOTED_IDENTIFIER.
const maxVarchar2 = 4000

// TypeSQL maps a storm type to Oracle, or says why it cannot.
func TypeSQL(table string, c *schema.Column) (string, error) {
	t := c.Type
	if t.Array {
		return "", unsupported(table, c, "Oracle has no array type",
			"store it as JSON, or normalise it into its own table")
	}
	if t.Enum {
		return "", fmt.Errorf(
			"oraddl: %s.%s is an enum; call TypeEnum with the schema's label list", table, c.Name)
	}
	switch t.Name {
	case schema.TypeBool:
		// Native, and 23c only. Measured rather than assumed:
		// internal/oraclespike confirmed it on the engine Oracle Free ships.
		// Before 23c this is NUMBER(1) plus a CHECK, which is a different
		// column and a different decoder.
		return "BOOLEAN", nil
	case schema.TypeInt2:
		return "NUMBER(5)", nil
	case schema.TypeInt4:
		return "NUMBER(10)", nil
	case schema.TypeInt8:
		return "NUMBER(19)", nil
	case schema.TypeFloat4:
		return "BINARY_FLOAT", nil
	case schema.TypeFloat8:
		return "BINARY_DOUBLE", nil
	case schema.TypeNumeric:
		if t.Precision > 0 {
			if t.Scale > 0 {
				return fmt.Sprintf("NUMBER(%d,%d)", t.Precision, t.Scale), nil
			}
			return fmt.Sprintf("NUMBER(%d)", t.Precision), nil
		}
		// ALLOWED here, where MySQL and SQL Server refuse it. A bare NUMBER is
		// Oracle's variable-scale form: 38 significant digits, fraction kept.
		// The refusal those two make is not about tidiness, it is that their
		// unspecified DECIMAL means scale ZERO and silently truncates every
		// fraction in an accounting column. Oracle's does not, so there is
		// nothing to refuse.
		return "NUMBER", nil
	case schema.TypeText:
		// CLOB, because VARCHAR2 stops at 4000 bytes by default and a storm
		// `text` column is unbounded by definition. The cost is real and worth
		// stating: a CLOB is a locator, not an inline value, so it cannot carry
		// a plain index and a driver fetches it separately.
		return "CLOB", nil
	case schema.TypeVarchar:
		if t.Size <= 0 {
			return "", unsupported(table, c,
				"Oracle has no unbounded VARCHAR2 — 4000 bytes is the limit unless the "+
					"database was created with MAX_STRING_SIZE=EXTENDED, which a library cannot assume",
				"declare a size: t.Col(&m.Name).Size(200), or use a text column, which becomes a CLOB")
		}
		if t.Size > maxVarchar2 {
			return "", unsupported(table, c,
				fmt.Sprintf("VARCHAR2 stops at %d and this column asks for %d", maxVarchar2, t.Size),
				"use a text column, which becomes a CLOB")
		}
		// CHAR semantics, spelled out. A bare VARCHAR2(n) counts BYTES, so a
		// name with an accent in it fits fewer characters than the model said —
		// and which characters fail depends on the database's character set.
		// A Go string is characters.
		return fmt.Sprintf("VARCHAR2(%d CHAR)", t.Size), nil
	case schema.TypeBytea:
		return "BLOB", nil
	case schema.TypeUUID:
		// RAW(16), which is 16 bytes and not 36. Oracle has no uuid type; this
		// is the same trade MySQL makes with BINARY(16), and SYS_GUID() fills
		// it server-side.
		return "RAW(16)", nil
	case schema.TypeTimestamptz:
		return "TIMESTAMP(6) WITH TIME ZONE", nil
	case schema.TypeTimestamp:
		return "TIMESTAMP(6)", nil
	case schema.TypeDate:
		// Oracle's DATE carries a TIME as well, to the second. A storm `date`
		// column stored here holds midnight, and a comparison that assumes
		// date-only equality still works because everything storm writes has
		// a zero time — but a row written by something else may not.
		return "DATE", nil

	// ---- the ones that do not cross ----
	case schema.TypeTime:
		return "", unsupported(table, c,
			"Oracle has no time-of-day type: DATE and TIMESTAMP both carry a date, and "+
				"INTERVAL DAY TO SECOND is a duration rather than a clock reading",
			"store it as INTERVAL DAY(0) TO SECOND(6) if it is a duration, or as the "+
				"number of seconds since midnight")
	case schema.TypeJSONB, schema.TypeJSON:
		// Native from 21c, and binary — the same OSON representation on disk
		// that PostgreSQL's jsonb has, rather than the reparsed text a CLOB
		// would be.
		return "JSON", nil
	case schema.TypeInterval:
		// Oracle HAS intervals, and that is exactly the problem: it has TWO,
		// and neither is PostgreSQL's. A PostgreSQL interval carries months
		// AND days AND microseconds in one value; INTERVAL YEAR TO MONTH drops
		// the days, INTERVAL DAY TO SECOND drops the months. Choosing one
		// silently discards a component.
		return "", unsupported(table, c,
			"Oracle splits intervals into YEAR TO MONTH and DAY TO SECOND, and a PostgreSQL "+
				"interval carries both — either choice drops a component silently",
			"store it as NUMBER microseconds, or as whichever of the two Oracle types "+
				"the column actually means, declared as that type")
	case schema.TypeInet, schema.TypeCIDR:
		return "", unsupported(table, c, "Oracle has no network address type",
			"store it as RAW(16), or as VARCHAR2(45 CHAR)")
	case schema.TypeMacaddr:
		return "", unsupported(table, c, "Oracle has no macaddr type",
			"store it as RAW(6) or VARCHAR2(17 CHAR)")
	case schema.TypeTSVector:
		return "", unsupported(table, c,
			"Oracle has no tsvector; its full-text search is an Oracle Text index over the "+
				"source columns, not a materialised column",
			"drop the column and declare a full-text index on the text columns instead")
	case schema.TypeTstzRange:
		return "", unsupported(table, c,
			"Oracle has no range types, and therefore no exclusion constraints",
			"store two TIMESTAMP WITH TIME ZONE columns and enforce non-overlap in the "+
				"application, knowing it can race")
	case schema.TypeHstore:
		return "", unsupported(table, c, "Oracle has no hstore", "use a JSON column")
	}
	return "", unsupported(table, c, "no Oracle equivalent is known for "+t.Name, "")
}

// oracleDefault translates the defaults storm generates.
func oracleDefault(c *schema.Column) string {
	switch strings.ToLower(strings.TrimSpace(c.Default)) {
	case "now()", "current_timestamp":
		return "SYSTIMESTAMP"
	case "gen_random_uuid()", "uuidv7()":
		// NO default, and the key is generated CLIENT-SIDE — the same answer
		// MySQL gives, reached from a different direction.
		//
		// SYS_GUID() exists and fills a RAW(16), so a server default LOOKS
		// available. Two things say otherwise. It is not a uuid at all: Oracle
		// documents it as host-and-sequence derived, so it is neither the
		// version 4 that `gen_random_uuid()` promises nor the time-ordered
		// version 7 that `uuidv7()` does, and a value that is guessable and
		// unsorted where the model asked for random or ordered is worse than
		// no value. And reading a server-generated key back needs
		// `RETURNING ... INTO`, which binds OUTPUT parameters that
		// runtime.Executor does not carry — see compile/oracle's KeysAreClientSide.
		//
		// The unit of work needs ids before the rows exist anyway.
		return ""
	}
	return c.Default
}

// CreateIndex renders a secondary index.
//
// A PARTIAL index becomes a function-based one. Oracle has no `WHERE` on an
// index, and its answer is better than it looks: a row whose key expression is
// entirely NULL is NOT indexed, so `(CASE WHEN <pred> THEN col END)` indexes
// exactly the rows a partial index would have. That makes soft delete's
// live-scoped unique port here — the third target in a row where it does, which
// leaves MySQL the odd one out rather than PostgreSQL. Measured:
// internal/oraclespike, result 1.
func CreateIndex(t *schema.Table, ix *schema.Index) string {
	var b strings.Builder
	b.WriteString("CREATE ")
	if ix.Unique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX " + Ident(indexName(t, ix)) + " ON " + Ident(t.Name) + " (")
	parts := make([]string, len(ix.Columns))
	for i, c := range ix.Columns {
		key := Ident(c.Name)
		if c.Expr {
			key = "(" + c.Name + ")"
		}
		if w := indexWhere(ix); w != "" {
			key = "CASE WHEN " + w + " THEN " + key + " END"
		}
		if c.Desc {
			key += " DESC"
		}
		parts[i] = key
	}
	b.WriteString(strings.Join(parts, ", "))
	b.WriteString(")")
	return b.String()
}

// indexWhere is the partial predicate, in ORACLE's spelling.
//
// A DECLARED Where is the model's own SQL and is passed through unchanged —
// rewriting somebody's expression is guesswork. Storm's OWN live-rows
// predicate is not: it is written as a BARE column name, which folds to
// lowercase on PostgreSQL and matches, and folds UP here, where the column
// storm created is quoted lowercase. `deleted_at IS NULL` is ORA-00904 against
// "deleted_at".
//
// So when schema.Index says the predicate is storm's, this builds it instead.
// The same cut checkExpr makes for an arc's CHECK.
func indexWhere(ix *schema.Index) string {
	if ix.LiveCol != "" {
		return Ident(ix.LiveCol) + " IS NULL"
	}
	return ix.Where
}

// AddForeignKey renders an ALTER TABLE ... ADD CONSTRAINT.
//
// ON UPDATE is not emitted, ever. Oracle has no ON UPDATE clause on a foreign
// key at all — not CASCADE, not RESTRICT, not NO ACTION — and the only way to
// get one is a trigger. Check refuses a model that asks for one rather than
// dropping it here, because a constraint silently weaker than the model said is
// the failure this whole package exists to prevent.
func AddForeignKey(t *schema.Table, fk *schema.ForeignKey) string {
	var b strings.Builder
	b.WriteString("ALTER TABLE " + Ident(t.Name) + " ADD CONSTRAINT " + Ident(fk.Name))
	b.WriteString(" FOREIGN KEY (" + identList(fk.Columns) + ")")
	b.WriteString(" REFERENCES " + Ident(fk.RefTable) + " (" + identList(fk.RefColumns) + ")")
	// CASCADE and SET NULL are the only two Oracle spells. NO ACTION and
	// RESTRICT are both "refuse the delete", which is the default and is
	// written by saying nothing.
	switch fk.OnDelete {
	case schema.Cascade:
		b.WriteString(" ON DELETE CASCADE")
	case schema.SetNull:
		b.WriteString(" ON DELETE SET NULL")
	}
	return b.String()
}

// Ident quotes an identifier with double quotes, which is Oracle's spelling —
// and which also PRESERVES CASE. An unquoted name folds to UPPER here, where
// PostgreSQL folds it to lower, so quoting is what keeps a storm model's
// lowercase names lowercase in the catalogue. Measured:
// internal/oraclespike's TestUnquotedIdentifiersFoldUp, which found that
// `fold_probe` and "fold_probe" are two different tables.
func Ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quoteLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func identList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = Ident(n)
	}
	return strings.Join(q, ", ")
}

func indexName(t *schema.Table, ix *schema.Index) string {
	if ix.Name != "" {
		return ix.Name
	}
	cols := make([]string, len(ix.Columns))
	for i, c := range ix.Columns {
		cols[i] = c.Name
	}
	return "ix_" + t.Name + "_" + strings.Join(cols, "_")
}

// checkExpr is a declared check's own SQL, passed through unchanged. Rewriting
// somebody's expression is guesswork, and the one exception is an arc's check,
// which storm wrote and whose PostgreSQL spelling casts a boolean with `::int`.
func checkExpr(ck *schema.Check) string {
	if len(ck.Arc) == 0 {
		return ck.Expr
	}
	parts := make([]string, len(ck.Arc))
	for i, col := range ck.Arc {
		parts[i] = "CASE WHEN " + Ident(col) + " IS NULL THEN 0 ELSE 1 END"
	}
	return strings.Join(parts, " + ") + " = 1"
}

func unsupported(table string, c *schema.Column, why, fix string) error {
	msg := fmt.Sprintf("%s.%s: %s", table, c.Name, why)
	if fix != "" {
		msg += "\n      " + fix
	}
	return fmt.Errorf("%s", msg)
}

func at(pos string) string {
	if pos == "" {
		return ""
	}
	return pos + ": "
}

func colPos(t *schema.Table, c *schema.Column) string {
	if c.Pos != "" {
		return c.Pos
	}
	return t.Pos
}

// maxIdent is Oracle 12.2 and later. Before that it was 30, which is shorter
// than `ck_<table>_<column>` on any table with a real name — and is why this is
// checked rather than assumed. Measured: internal/oraclespike, result 1.
const maxIdent = 128
