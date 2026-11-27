// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package liteddl renders a storm schema as SQLite DDL, or says why it cannot.
//
// SQLite is the first target whose differences are mostly about STORAGE rather
// than syntax. Its grammar is close to PostgreSQL's — double-quoted
// identifiers, partial and expression indexes, RETURNING, ON CONFLICT with a
// target — but a column's declared type is a hint, not a contract: a value is
// stored as whatever it looks like (the "type affinity" rules), and a length
// is never enforced. So most of what this package decides is how to make a
// column hold what the model says it holds:
//
//   - A varchar(n) carries `CHECK (length(c) <= n)`. SQLite parses the n and
//     ignores it; PostgreSQL refuses the 201st character. Without the check a
//     value too long for every other target is accepted here, and the first
//     sign of it is a migration to another engine that fails on the data.
//   - A json or jsonb column is TEXT with `CHECK (json_valid(c))`. A column
//     DECLARED json has NUMERIC affinity, which stores the document `123` as
//     the integer 123 — and a decoder asking for the document's bytes then
//     gets none. TEXT affinity keeps every document as the text it was.
//   - A numeric is NUMERIC(p,s), which SQLite stores as a 64-bit integer or a
//     64-bit float. That is exact to fifteen significant digits and no
//     further, so a numeric with more, or with no declared precision at all,
//     is REFUSED by Check rather than stored with digits missing.
//   - A uuid is a 16-byte BLOB under the declared type UUID, which keeps the
//     key 16 bytes wide rather than 36 and keeps the type readable on the way
//     back in. There is no uuid function in SQLite, so the key is generated
//     client-side — the answer MySQL and Oracle give too.
//   - A timestamp is TEXT, in one fixed-width UTC form that runtime/sqlitedrv
//     writes and SQLite's own date functions read:
//     2006-01-02T15:04:05.000000000Z. Fixed-width matters: SQLite compares
//     these as text, and text sorts chronologically only when every value has
//     the same shape.
//   - An enum is TEXT plus a CHECK listing the labels, named
//     ck_<table>_<column> as it is on SQL Server and Oracle, which have no
//     enum type either.
//
// FOREIGN KEYS ARE PART OF THE TABLE. SQLite has no ALTER TABLE … ADD
// CONSTRAINT of any kind, so a key cannot be added after the table exists.
// They are rendered inline, which needs no ordering between tables: SQLite
// resolves a REFERENCES clause when a row is written, not when the table is
// created. Changing one later is a table rebuild — see Rebuild.
package liteddl

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/v2/schema"
)

// Statements renders a schema as the individual statements to run, each
// without its terminator, or reports every reason it cannot.
func Statements(s *schema.Schema) ([]string, error) { return StatementsIn(s, "") }

// StatementsIn is Statements with every table and index created in the
// attached database ns rather than main. It is how migrate normalises a model:
// the scratch copy is applied to an in-memory database attached beside the
// real one, read back through the same introspector, and detached.
func StatementsIn(s *schema.Schema, ns string) ([]string, error) {
	if err := Check(s); err != nil {
		return nil, err
	}
	enums := enumsOf(s)
	var out []string
	for _, t := range s.Tables {
		def, err := CreateTableIn(ns, t, enums)
		if err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	for _, t := range s.Tables {
		out = append(out, IndexesIn(ns, t)...)
	}
	return out, nil
}

// Create renders a schema as a migration file.
func Create(s *schema.Schema) (string, error) {
	stmts, err := Statements(s)
	if err != nil {
		return "", err
	}
	if len(stmts) == 0 {
		return "", nil
	}
	return strings.Join(stmts, ";\n\n") + ";\n", nil
}

func enumsOf(s *schema.Schema) map[string]*schema.Enum {
	m := make(map[string]*schema.Enum, len(s.Enums))
	for _, e := range s.Enums {
		m[e.Name] = e
	}
	return m
}

// Ident quotes an identifier. SQLite folds nothing — names compare
// case-insensitively for ASCII and are stored as written — so quoting is about
// keywords and punctuation rather than case, and a model's `order` column is a
// column rather than a syntax error.
func Ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// qualified names an object in the attached database ns, or in main.
func qualified(ns, name string) string {
	if ns == "" {
		return Ident(name)
	}
	return Ident(ns) + "." + Ident(name)
}

func quoteLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func identList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = Ident(n)
	}
	return strings.Join(q, ", ")
}

// CreateTable renders one table with every constraint it has: its key, its
// uniques, its checks and its foreign keys. Indexes are separate statements;
// see Indexes.
func CreateTable(t *schema.Table, enums map[string]*schema.Enum) (string, error) {
	return CreateTableIn("", t, enums)
}

// CreateTableIn is CreateTable in the attached database ns.
func CreateTableIn(ns string, t *schema.Table, enums map[string]*schema.Enum) (string, error) {
	return createTableNamed(qualified(ns, t.Name), t, enums)
}

func createTableNamed(name string, t *schema.Table, enums map[string]*schema.Enum) (string, error) {
	var b strings.Builder
	b.WriteString("CREATE TABLE " + name + " (\n")
	parts := make([]string, 0, len(t.Columns)+4)
	rowid := RowidKey(t)
	for _, c := range t.Columns {
		def, err := ColumnDef(t, c, enums)
		if err != nil {
			return "", err
		}
		parts = append(parts, "    "+def)
	}
	if len(t.PrimaryKey) > 0 && rowid == "" {
		parts = append(parts, "    PRIMARY KEY ("+identList(t.PrimaryKey)+")")
	}
	for _, u := range t.Uniques {
		if !plainColumns(t, u.Columns) {
			continue // an index, rendered by Indexes: see uniqueIndex
		}
		parts = append(parts, "    CONSTRAINT "+Ident(u.Name)+" UNIQUE ("+identList(u.Columns)+")")
	}
	for _, c := range t.Columns {
		if !c.Type.Enum {
			continue
		}
		if e := enums[c.Type.Name]; e != nil {
			parts = append(parts, "    CONSTRAINT "+Ident(EnumCheckName(t.Name, c.Name))+
				" CHECK ("+EnumCheck(c.Name, e)+")")
		}
	}
	for _, ck := range t.Checks {
		parts = append(parts, "    CONSTRAINT "+Ident(ck.Name)+" CHECK ("+CheckExpr(ck)+")")
	}
	for _, fk := range t.ForeignKeys {
		parts = append(parts, "    "+ForeignKeyDef(fk))
	}
	b.WriteString(strings.Join(parts, ",\n"))
	b.WriteString("\n)")
	return b.String(), nil
}

// RowidKey names the column that is the table's rowid, or "".
//
// SQLite gives every table a 64-bit rowid, and a column declared exactly
// `INTEGER PRIMARY KEY` IS that rowid rather than a second copy of it — the
// only way a key here is generated by the server. storm's identity and serial
// columns become one, with AUTOINCREMENT, which is what stops a deleted key
// being handed out again: without it SQLite reuses the largest one, and a
// reference somebody kept to a deleted row starts pointing at a new one.
func RowidKey(t *schema.Table) string {
	if len(t.PrimaryKey) != 1 {
		return ""
	}
	c := t.Column(t.PrimaryKey[0])
	if c == nil || !(c.Identity || c.Serial) || !isInteger(c.Type) {
		return ""
	}
	return c.Name
}

func isInteger(t schema.Type) bool {
	if t.Array || t.Enum {
		return false
	}
	switch t.Name {
	case schema.TypeInt2, schema.TypeInt4, schema.TypeInt8:
		return true
	}
	return false
}

// plainColumns reports whether every entry of a unique's column list is a
// column of t. A table constraint takes only names here; a unique over an
// expression is a unique INDEX instead.
func plainColumns(t *schema.Table, cols []string) bool {
	for _, c := range cols {
		if t.Column(c) == nil {
			return false
		}
	}
	return true
}

// EnumCheckName is what an enum column's constraint is called — derived, as
// msddl and oraddl derive it, so a diff that renders it twice arrives at the
// same name twice.
func EnumCheckName(table, col string) string { return "ck_" + table + "_" + col }

// EnumCheck is the constraint that makes a text column an enum.
func EnumCheck(col string, e *schema.Enum) string {
	vals := make([]string, len(e.Labels))
	for i, l := range e.Labels {
		vals[i] = quoteLit(l)
	}
	return Ident(col) + " IN (" + strings.Join(vals, ", ") + ")"
}

// CheckExpr is a declared check's own SQL, passed through: rewriting somebody's
// expression is guesswork. The exception is an arc's, which storm wrote and
// whose PostgreSQL spelling casts a boolean with `::int` — a syntax error here,
// where a comparison is already the integer 0 or 1.
func CheckExpr(ck *schema.Check) string {
	if len(ck.Arc) == 0 {
		return ck.Expr
	}
	parts := make([]string, len(ck.Arc))
	for i, col := range ck.Arc {
		parts[i] = "(" + Ident(col) + " IS NOT NULL)"
	}
	sum := strings.Join(parts, " + ")
	if ck.ArcOptional {
		return sum + " <= 1"
	}
	return sum + " = 1"
}

// ForeignKeyDef is the table constraint for one foreign key.
func ForeignKeyDef(fk *schema.ForeignKey) string {
	var b strings.Builder
	b.WriteString("CONSTRAINT " + Ident(fk.Name))
	b.WriteString(" FOREIGN KEY (" + identList(fk.Columns) + ")")
	b.WriteString(" REFERENCES " + Ident(fk.RefTable) + " (" + identList(fk.RefColumns) + ")")
	if a := action(fk.OnDelete); a != "" {
		b.WriteString(" ON DELETE " + a)
	}
	if a := action(fk.OnUpdate); a != "" {
		b.WriteString(" ON UPDATE " + a)
	}
	if fk.Deferrable {
		b.WriteString(" DEFERRABLE INITIALLY DEFERRED")
	}
	return b.String()
}

// action spells a referential action, or "" for NO ACTION, which is the
// default and is written by saying nothing.
func action(a schema.Action) string {
	switch a {
	case schema.Cascade:
		return "CASCADE"
	case schema.SetNull:
		return "SET NULL"
	case schema.Restrict:
		return "RESTRICT"
	case schema.SetDefault:
		return "SET DEFAULT"
	}
	return ""
}

// ColumnDef renders one column of t.
func ColumnDef(t *schema.Table, c *schema.Column, enums map[string]*schema.Enum) (string, error) {
	ty, err := typeOf(t.Name, c, enums)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(Ident(c.Name) + " " + ty)
	if RowidKey(t) == c.Name {
		// Exactly INTEGER, whatever the model's width: `BIGINT PRIMARY KEY`
		// is an ordinary column beside the rowid, not the rowid, and would
		// never be filled by the server.
		return Ident(c.Name) + " INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL", nil
	}
	if c.Generated != "" {
		// STORED, which is what PostgreSQL's generated columns are. VIRTUAL is
		// SQLite's default and is computed on every read.
		b.WriteString(" GENERATED ALWAYS AS (" + c.Generated + ") STORED")
	} else if d := Default(c); d != "" {
		b.WriteString(" DEFAULT " + d)
	}
	if c.NotNull {
		b.WriteString(" NOT NULL")
	}
	if ck := columnCheck(c); ck != "" {
		b.WriteString(" CHECK (" + ck + ")")
	}
	return b.String(), nil
}

// columnCheck is the constraint a column's TYPE implies here, or "". See the
// package comment: SQLite enforces neither a length nor a document, so the
// type says it and this makes it true.
func columnCheck(c *schema.Column) string {
	if c.Type.Array || c.Type.Enum {
		return ""
	}
	switch c.Type.Name {
	case schema.TypeVarchar:
		if c.Type.Size > 0 {
			return "length(" + Ident(c.Name) + ") <= " + strconv.Itoa(c.Type.Size)
		}
	case schema.TypeJSON, schema.TypeJSONB:
		return "json_valid(" + Ident(c.Name) + ")"
	}
	return ""
}

// LengthCheck and JSONCheck are columnCheck's two shapes, exported so the
// introspector can recognise them on the way back in and read them as the
// TYPE they came from rather than as checks a model declared.
func LengthCheck(col string, n int) string {
	return "length(" + Ident(col) + ") <= " + strconv.Itoa(n)
}

// JSONCheck is the document check.
func JSONCheck(col string) string { return "json_valid(" + Ident(col) + ")" }

// typeOf is TypeSQL with the enum labels to hand.
func typeOf(table string, c *schema.Column, enums map[string]*schema.Enum) (string, error) {
	if c.Type.Enum {
		if enums[c.Type.Name] == nil {
			return "", fmt.Errorf("liteddl: %s.%s is an enum and %s is not declared in the schema",
				table, c.Name, c.Type.Name)
		}
		return "TEXT", nil
	}
	return TypeSQL(table, c)
}

// MaxNumericPrecision is how many significant digits a NUMERIC holds exactly
// here. SQLite stores one as a 64-bit float or integer, and a float carries
// fifteen decimal digits that survive a round trip — the rest are noise.
const MaxNumericPrecision = 15

// TimestampFormat is the one shape every temporal is stored in: UTC, with a
// fixed nine-digit fraction, so that text order is time order. runtime/sqlitedrv
// formats a bound time.Time with it and Now produces it.
const TimestampFormat = "2006-01-02T15:04:05.000000000Z"

// Now is the server's clock in TimestampFormat. strftime's %f is milliseconds,
// so the fraction is padded to nine digits rather than left three wide — a
// value three digits wide sorts BEFORE every nine-digit value of the same
// millisecond, and `deleted_at <= now` would miss the row deleted this instant.
const Now = "strftime('%Y-%m-%dT%H:%M:%f000000Z', 'now')"

// TypeSQL maps a storm type to the type SQLite stores it as, or says why it
// cannot. See the package comment for why each one is what it is.
func TypeSQL(table string, c *schema.Column) (string, error) {
	t := c.Type
	if t.Array {
		return "", unsupported(table, c, "SQLite has no array type",
			"store it as JSON (a jsonb column), or normalise it into its own table")
	}
	if t.Enum {
		return "TEXT", nil
	}
	switch t.Name {
	case schema.TypeBool:
		return "BOOLEAN", nil
	case schema.TypeInt2:
		return "SMALLINT", nil
	case schema.TypeInt4:
		return "INTEGER", nil
	case schema.TypeInt8:
		return "BIGINT", nil
	case schema.TypeFloat4:
		return "REAL", nil
	case schema.TypeFloat8:
		return "DOUBLE", nil
	case schema.TypeNumeric:
		if t.Precision <= 0 {
			return "", unsupported(table, c,
				"SQLite stores a NUMERIC as a 64-bit float or integer, exact to fifteen "+
					"significant digits, and an unbounded numeric promises more",
				fmt.Sprintf("declare the precision: t.Col(&m.Price).Numeric(15, 2) — at most %d digits",
					MaxNumericPrecision))
		}
		if t.Precision > MaxNumericPrecision {
			return "", unsupported(table, c,
				fmt.Sprintf("SQLite stores a NUMERIC as a 64-bit float or integer, exact to %d "+
					"significant digits, and numeric(%d,%d) needs %d", MaxNumericPrecision,
					t.Precision, t.Scale, t.Precision),
				"narrow it to numeric(15, s) or fewer digits, or store the amount in minor "+
					"units as an int8")
		}
		if t.Scale > 0 {
			return fmt.Sprintf("NUMERIC(%d,%d)", t.Precision, t.Scale), nil
		}
		return fmt.Sprintf("NUMERIC(%d)", t.Precision), nil
	case schema.TypeText:
		return "TEXT", nil
	case schema.TypeVarchar:
		if t.Size > 0 {
			return fmt.Sprintf("VARCHAR(%d)", t.Size), nil
		}
		return "TEXT", nil
	case schema.TypeBytea:
		return "BLOB", nil
	case schema.TypeUUID:
		return "UUID", nil
	case schema.TypeTimestamptz:
		return "TIMESTAMP", nil
	case schema.TypeTimestamp:
		return "DATETIME", nil
	case schema.TypeDate:
		return "DATE", nil
	case schema.TypeTime:
		return "TIME", nil
	case schema.TypeJSON, schema.TypeJSONB:
		return "TEXT", nil

	// ---- the ones that do not cross ----
	case schema.TypeInterval:
		return "", unsupported(table, c,
			"SQLite has no interval type, and a PostgreSQL interval carries months, days "+
				"and microseconds that no single number holds",
			"store the duration as an int8 of microseconds, or the months and the "+
				"microseconds as two columns")
	case schema.TypeInet, schema.TypeCIDR:
		return "", unsupported(table, c, "SQLite has no network address type",
			"store it as a 16-byte bytea, or as text")
	case schema.TypeMacaddr:
		return "", unsupported(table, c, "SQLite has no macaddr type",
			"store it as a 6-byte bytea, or as text")
	case schema.TypeTSVector:
		return "", unsupported(table, c,
			"SQLite has no tsvector; its full-text search is an FTS5 virtual table, "+
				"which is a second table rather than a column",
			"drop the column, and search through an FTS5 table you maintain")
	case schema.TypeTstzRange:
		return "", unsupported(table, c,
			"SQLite has no range types, and therefore no exclusion constraints",
			"store the two bounds as two timestamptz columns")
	case schema.TypeHstore:
		return "", unsupported(table, c, "SQLite has no hstore", "use a jsonb column")
	}
	return "", unsupported(table, c, "no SQLite equivalent is known for "+t.Name, "")
}

// ColumnType is the type a lowering NAMES for a column, for the places that
// have to — a key unpacked from a bound JSON list. A type TypeSQL refuses has
// no spelling here; that cannot reach a generated package, because the
// generator runs Check first.
func ColumnType(c *schema.Column) string {
	if c.Type.Enum {
		return "TEXT"
	}
	if t, err := TypeSQL("", c); err == nil {
		return t
	}
	return "TEXT"
}

// Default translates the defaults storm generates, and passes the model's own
// through. "" means the column has none here.
func Default(c *schema.Column) string {
	d := strings.TrimSpace(c.Default)
	switch strings.ToLower(d) {
	case "":
		return ""
	case "now()", "current_timestamp", "current_timestamp()", "localtimestamp", "transaction_timestamp()":
		return "(" + Now + ")"
	case "gen_random_uuid()", "uuidv7()", "uuid_generate_v4()":
		// None: SQLite has no uuid function, so the key is storm's to
		// generate — see compile/sqlite's KeysAreClientSide.
		return ""
	case "true":
		return "1"
	case "false":
		return "0"
	case "null":
		return "NULL"
	}
	d = stripCast(d)
	if isConstant(d) {
		return d
	}
	// An expression default must be parenthesised here, where PostgreSQL
	// takes it bare.
	return "(" + d + ")"
}

// stripCast removes PostgreSQL's trailing `::type` from a literal default —
// `'{}'::jsonb`, `0::numeric` — which is a syntax error here. Only a cast on a
// whole literal is removed: anything else is the model's expression and is
// left for SQLite to refuse, which names the problem better than a guess.
func stripCast(d string) string {
	i := strings.LastIndex(d, "::")
	if i < 0 {
		return d
	}
	head, typ := d[:i], d[i+2:]
	for _, r := range typ {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '_' || r == ' ' || r == '(' || r == ')' || r == ',') {
			return d
		}
	}
	if isConstant(head) {
		return head
	}
	return d
}

// isConstant reports whether d is a literal SQLite takes unparenthesised as a
// default: a string, a number, NULL, a boolean or a blob.
func isConstant(d string) bool {
	if d == "" {
		return false
	}
	switch strings.ToUpper(d) {
	case "NULL", "TRUE", "FALSE", "CURRENT_TIMESTAMP", "CURRENT_DATE", "CURRENT_TIME":
		return true
	}
	if len(d) >= 2 && d[0] == '\'' && d[len(d)-1] == '\'' {
		return !strings.Contains(strings.ReplaceAll(d[1:len(d)-1], "''", ""), "'")
	}
	if (d[0] == 'x' || d[0] == 'X') && len(d) >= 3 && d[1] == '\'' && d[len(d)-1] == '\'' {
		return true
	}
	s := d
	if s[0] == '-' || s[0] == '+' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return !strings.ContainsAny(s, "xXpP_")
	}
	return false
}

// Indexes renders every secondary index of t, and every unique over an
// expression, as CREATE INDEX statements.
func Indexes(t *schema.Table) []string { return IndexesIn("", t) }

// IndexesIn is Indexes in the attached database ns.
func IndexesIn(ns string, t *schema.Table) []string {
	var out []string
	for _, u := range t.Uniques {
		if plainColumns(t, u.Columns) {
			continue
		}
		out = append(out, createIndex(ns, t, uniqueIndex(u)))
	}
	for _, ix := range t.Indexes {
		out = append(out, createIndex(ns, t, ix))
	}
	return out
}

// uniqueIndex is a unique constraint over an expression as the unique index it
// has to be here: a UNIQUE table constraint takes column names only.
func uniqueIndex(u *schema.Unique) *schema.Index {
	ix := &schema.Index{Name: u.Name, Unique: true}
	for _, c := range u.Columns {
		ix.Columns = append(ix.Columns, schema.IndexColumn{Name: c, Expr: true})
	}
	return ix
}

// CreateIndex renders one index of t.
func CreateIndex(t *schema.Table, ix *schema.Index) string { return createIndex("", t, ix) }

func createIndex(ns string, t *schema.Table, ix *schema.Index) string {
	var b strings.Builder
	b.WriteString("CREATE ")
	if ix.Unique {
		b.WriteString("UNIQUE ")
	}
	// The SCHEMA goes on the index name and never on the table: an index
	// lives in its table's database, and `ON "ns"."t"` is a syntax error.
	b.WriteString("INDEX " + qualified(ns, IndexName(t, ix)) + " ON " + Ident(t.Name) + " (")
	var parts []string
	for _, c := range ix.Columns {
		key := Ident(c.Name)
		if c.Expr {
			key = "(" + c.Name + ")"
		}
		if c.Collate != "" {
			key += " COLLATE " + Collation(c.Collate)
		}
		if c.Desc {
			key += " DESC"
		}
		// NULLS FIRST/LAST has no place in an index here, and it changes no
		// answer: the order a query returns is its own ORDER BY's.
		parts = append(parts, key)
	}
	// INCLUDE is widened to trailing keys, which is what a covering index IS
	// on an engine without the clause — the same widening MySQL gets. Check
	// refuses it on a UNIQUE index, where trailing keys would change what
	// counts as a duplicate.
	for _, inc := range ix.Include {
		parts = append(parts, Ident(inc))
	}
	b.WriteString(strings.Join(parts, ", "))
	b.WriteString(")")
	if w := IndexWhere(ix); w != "" {
		b.WriteString(" WHERE " + w)
	}
	return b.String()
}

// IndexWhere is a partial index's predicate: storm's own live-rows one, or the
// model's, passed through.
func IndexWhere(ix *schema.Index) string {
	if ix.LiveCol != "" {
		return Ident(ix.LiveCol) + " IS NULL"
	}
	return ix.Where
}

// DropIndex removes an index. Index names are per DATABASE here, not per
// table, so no table is named.
func DropIndex(t *schema.Table, ix *schema.Index) string {
	return "DROP INDEX " + Ident(IndexName(t, ix))
}

// IndexName is an index's name, derived when the model gave none.
func IndexName(t *schema.Table, ix *schema.Index) string {
	if ix.Name != "" {
		return ix.Name
	}
	cols := make([]string, len(ix.Columns))
	for i, c := range ix.Columns {
		cols[i] = c.Name
	}
	return "ix_" + t.Name + "_" + strings.Join(cols, "_")
}

// Collation maps a collation name to SQLite's. SQLite has three built in —
// BINARY, NOCASE and RTRIM — and PostgreSQL's "C" is BINARY. Check refuses
// any other, since a collation SQLite does not have is an error at CREATE
// INDEX.
func Collation(name string) string {
	switch strings.ToUpper(strings.Trim(name, `"`)) {
	case "C", "POSIX", "BINARY":
		return "BINARY"
	case "NOCASE":
		return "NOCASE"
	case "RTRIM":
		return "RTRIM"
	}
	return Ident(name)
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
