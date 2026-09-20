// Package msddl renders a schema as SQL Server DDL.
//
// The third back end. What compile/myddl is mostly about — the types that do
// not cross — is a shorter list here: SQL Server has a native uuid, a native
// bit, precise date and time types, and a server-side uuid DEFAULT, so the
// default storm.Model ports without a single client-side substitution.
//
// Two things it has that MySQL does not are worth naming, because they undo
// refusals M9 had to make:
//
//   - FILTERED INDEXES. `CREATE UNIQUE INDEX ... WHERE` exists, so a soft-delete
//     table's live-scoped unique ports, and so does a polymorphic arc's
//     per-variant index. myddl.Check refuses the first and widens the second.
//   - COVERING INDEXES. `INCLUDE` exists, so an index that carries payload
//     columns is not rewritten as trailing keys.
//
// And one inversion that is a correctness matter rather than a feature:
// PostgreSQL's UNIQUE treats NULLs as DISTINCT, so a nullable unique column
// accepts many NULL rows. SQL Server's treats them as EQUAL and accepts exactly
// one. That difference changes ANSWERS, so it is translated rather than
// refused — see CreateIndex.
package msddl

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Create renders the whole schema.
//
// Returns an error rather than emitting something that will not run: a type
// with no SQL Server equivalent is a portability decision, and the moment to
// make it is now, not when a customer's install fails.
func Create(s *schema.Schema) (string, error) {
	if err := Check(s); err != nil {
		return "", err
	}
	enums := enumsOf(s)
	var b strings.Builder
	for _, t := range s.Tables {
		def, err := CreateTable(t, enums)
		if err != nil {
			return "", err
		}
		b.WriteString(def)
		b.WriteString("\n\n")
	}
	for _, t := range s.Tables {
		for _, ix := range t.Indexes {
			b.WriteString(CreateIndex(t, ix))
			b.WriteString("\n")
		}
	}
	for _, t := range s.Tables {
		for _, fk := range t.ForeignKeys {
			b.WriteString(AddForeignKey(t, fk))
			b.WriteString("\n")
		}
	}
	return b.String(), nil
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

// CreateTable renders one table.
//
// enums is the schema's declared labels, and it is a PARAMETER rather than
// something looked up later because an enum column cannot be rendered without
// them: this server has no enum type, so the column is a width and a CHECK, and
// both come from the label list. Passing nil renders a table with no enum
// columns and refuses one that has them — which is the honest answer for a
// caller that did not supply the labels.
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
	// An enum's CHECK is a TABLE-level constraint, so it is collected here
	// rather than appended to the column: SQL Server has no enum type, and the
	// set of accepted values is the whole of what the declaration means.
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
	b.WriteString("\n);")
	return b.String(), nil
}

// enumCheckName is what an enum column's constraint is called. Derived rather
// than declared, because the model never names it — and a migration that has to
// drop it needs the name to be the same every time.
func enumCheckName(table, col string) string { return "ck_" + table + "_" + col }

// ColumnDef renders one column.
func ColumnDef(table string, c *schema.Column, enums map[string]*schema.Enum) (string, error) {
	if c.Generated != "" {
		// A computed column names no type: SQL Server derives it from the
		// expression. PERSISTED is what makes it storable and indexable, which
		// is the only form storm generates — the expression is the model's own
		// SQL and is passed through unchanged, because rewriting somebody's
		// expression is guesswork.
		//
		// Unlike MariaDB, the nullability clause IS allowed after PERSISTED.
		def := Ident(c.Name) + " AS (" + c.Generated + ") PERSISTED"
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
	case c.Identity:
		// IDENTITY(1,1) rather than a sequence: it is the form that needs no
		// separate object, and OUTPUT hands the assigned value back, so nothing
		// has to ask for it afterwards.
		b.WriteString(" IDENTITY(1,1)")
	case c.Default != "":
		if d := mssqlDefault(c); d != "" {
			b.WriteString(" DEFAULT " + d)
		}
	}
	if c.NotNull {
		b.WriteString(" NOT NULL")
	}
	return b.String(), nil
}

// checkExpr is a check constraint's expression in SQL Server's spelling.
//
// A check the MODEL declared is passed through: it is the model's own SQL, and
// rewriting somebody's expression is guesswork. An ARC's check is storm's own,
// and its PostgreSQL spelling casts each comparison with `::int`, which parses
// nowhere else. SQL Server has no boolean-to-int coercion either — `(x IS NOT
// NULL)` is a predicate, not a value — so each arm becomes a CASE.
func checkExpr(ck *schema.Check) string {
	if len(ck.Arc) == 0 {
		return ck.Expr
	}
	var b strings.Builder
	for i, c := range ck.Arc {
		if i > 0 {
			b.WriteString(" + ")
		}
		b.WriteString("CASE WHEN " + Ident(c) + " IS NOT NULL THEN 1 ELSE 0 END")
	}
	if ck.ArcOptional {
		b.WriteString(" <= 1")
	} else {
		b.WriteString(" = 1")
	}
	return b.String()
}

// ColumnType is the type SQL Server will store a column AS, with the enum
// labels to hand. It is what migrate needs: an ALTER COLUMN restates the type
// on every change, including a change that is only about nullability, so the
// diff has to be able to ask for a column's type without rendering the column.
func ColumnType(table string, c *schema.Column, enums map[string]*schema.Enum) (string, error) {
	return typeOf(table, c, enums)
}

// typeOf is TypeSQL with the enum labels to hand.
//
// TypeSQL refuses an enum by design — it has no labels — and for a long time
// nothing supplied them, so Check accepted a model with an enum column and
// Create then refused it. A check that says a model ports and a generator that
// says it does not is the one disagreement this package exists to prevent.
func typeOf(table string, c *schema.Column, enums map[string]*schema.Enum) (string, error) {
	if c.Type.Enum {
		e := enums[c.Type.Name]
		if e == nil {
			return "", fmt.Errorf("msddl: %s.%s is an enum and %s is not declared in the schema",
				table, c.Name, c.Type.Name)
		}
		return TypeEnum(e), nil
	}
	return TypeSQL(table, c)
}

// TypeSQL maps a storm type to SQL Server, or says why it cannot.
func TypeSQL(table string, c *schema.Column) (string, error) {
	t := c.Type
	if t.Array {
		return "", unsupported(table, c, "SQL Server has no array type",
			"store it as JSON in an NVARCHAR(MAX), or normalise it into its own table")
	}
	if t.Enum {
		return "", fmt.Errorf(
			"msddl: %s.%s is an enum; call TypeEnum with the schema's label list", table, c.Name)
	}
	switch t.Name {
	case schema.TypeBool:
		return "BIT", nil
	case schema.TypeInt2:
		return "SMALLINT", nil
	case schema.TypeInt4:
		return "INT", nil
	case schema.TypeInt8:
		return "BIGINT", nil
	case schema.TypeFloat4:
		return "REAL", nil
	case schema.TypeFloat8:
		// FLOAT(53) spelled out: a bare FLOAT is 53 bits here, but FLOAT(n) for
		// n <= 24 silently becomes REAL, and a reader should not have to
		// remember which side of the boundary the default falls on.
		return "FLOAT(53)", nil
	case schema.TypeNumeric:
		if t.Precision > 0 {
			if t.Scale > 0 {
				return fmt.Sprintf("DECIMAL(%d,%d)", t.Precision, t.Scale), nil
			}
			return fmt.Sprintf("DECIMAL(%d)", t.Precision), nil
		}
		// Same refusal as MySQL's, for the same reason: SQL Server has no
		// unbounded DECIMAL, and an unspecified one means DECIMAL(18,0) — every
		// fraction truncated, silently, in an accounting column.
		return "", unsupported(table, c,
			"SQL Server has no unbounded DECIMAL, and an unspecified one means DECIMAL(18,0) — every fraction truncated",
			"declare the precision: t.Col(&m.Amount).Numeric(19, 4)")
	case schema.TypeText:
		// NVARCHAR, not VARCHAR: VARCHAR is bytes in the database's code page,
		// so what it stores depends on who ran setup. NVARCHAR is UTF-16 and
		// holds the same text everywhere, which is what a Go string is.
		return "NVARCHAR(MAX)", nil
	case schema.TypeVarchar:
		if t.Size > 0 {
			return fmt.Sprintf("NVARCHAR(%d)", t.Size), nil
		}
		return "NVARCHAR(MAX)", nil
	case schema.TypeBytea:
		return "VARBINARY(MAX)", nil
	case schema.TypeUUID:
		// Native, and 16 bytes. No BINARY(16) substitution and no CHAR(36).
		return "UNIQUEIDENTIFIER", nil
	case schema.TypeTimestamptz:
		// DATETIMEOFFSET carries the offset, which is what distinguishes it
		// from DATETIME2 and what a timestamptz column means. (7) is 100 ns,
		// the finest this type has.
		return "DATETIMEOFFSET(7)", nil
	case schema.TypeTimestamp:
		return "DATETIME2(7)", nil
	case schema.TypeDate:
		return "DATE", nil
	case schema.TypeTime:
		return "TIME(7)", nil
	case schema.TypeJSONB, schema.TypeJSON:
		// Through the 2019 level SQL Server has no json TYPE — it has json
		// FUNCTIONS over text. NVARCHAR(MAX) is the storable form and the one
		// OPENJSON and JSON_VALUE read; CheckJSON adds the ISJSON constraint
		// that makes it a document rather than any string at all.
		return "NVARCHAR(MAX)", nil

	// ---- the ones that do not cross ----
	case schema.TypeInterval:
		return "", unsupported(table, c, "SQL Server has no INTERVAL type",
			"store it as BIGINT microseconds, or as two DATETIMEOFFSET columns")
	case schema.TypeInet, schema.TypeCIDR:
		return "", unsupported(table, c, "SQL Server has no network address type",
			"store it as VARBINARY(16), or as NVARCHAR(45)")
	case schema.TypeMacaddr:
		return "", unsupported(table, c, "SQL Server has no macaddr type",
			"store it as BINARY(6) or NVARCHAR(17)")
	case schema.TypeTSVector:
		return "", unsupported(table, c,
			"SQL Server has no tsvector; its full-text search is a FULLTEXT INDEX over the source columns and a separate catalog, not a materialised column",
			"drop the column and declare a full-text index on the text columns instead")
	case schema.TypeTstzRange:
		return "", unsupported(table, c,
			"SQL Server has no range types, and therefore no exclusion constraints",
			"store two DATETIMEOFFSET columns and enforce non-overlap in the application, knowing it can race")
	case schema.TypeHstore:
		return "", unsupported(table, c, "SQL Server has no hstore",
			"use JSON in an NVARCHAR(MAX)")
	}
	return "", unsupported(table, c, "no SQL Server equivalent is known for "+t.Name, "")
}

// TypeEnum renders a storm enum.
//
// SQL Server has no ENUM type, so this is the translation every SQL Server
// schema uses: the widest label, and a CHECK that accepts exactly the declared
// set. The accepted values are the same ones MySQL's native ENUM accepts, which
// is what makes it a translation rather than an approximation — what differs is
// that the stored value is the text rather than an ordinal.
//
// The constraint is rendered by EnumCheck, because it is a table-level clause
// and this is the column's type.
func TypeEnum(e *schema.Enum) string {
	n := 1
	for _, l := range e.Labels {
		if len(l) > n {
			n = len(l)
		}
	}
	return fmt.Sprintf("NVARCHAR(%d)", n)
}

// EnumCheck is the constraint that makes a column hold only an enum's labels.
func EnumCheck(col string, e *schema.Enum) string {
	q := make([]string, len(e.Labels))
	for i, l := range e.Labels {
		q[i] = quoteLit(l)
	}
	return Ident(col) + " IN (" + strings.Join(q, ", ") + ")"
}

// at is a declaration's position as a message prefix, or nothing.
func at(pos string) string {
	if pos == "" {
		return ""
	}
	return pos + ": "
}

// colPos is the best position for a problem about a column.
func colPos(t *schema.Table, c *schema.Column) string {
	if c != nil && c.Pos != "" {
		return c.Pos
	}
	return t.Pos
}

// indexPos is the best position for a problem about an index.
func indexPos(t *schema.Table, ix *schema.Index) string {
	if len(ix.Columns) > 0 {
		if c := t.Column(ix.Columns[0].Name); c != nil && c.Pos != "" {
			return c.Pos
		}
	}
	return t.Pos
}

// Check reports every portability problem in one pass — all of them, not the
// first: finding them one deploy at a time is the failure mode this replaces.
func Check(s *schema.Schema) error {
	var problems []string
	enums := map[string]*schema.Enum{}
	for _, e := range s.Enums {
		enums[e.Name] = e
	}
	for _, t := range s.Tables {
		if len(t.Excludes) > 0 {
			problems = append(problems, fmt.Sprintf(
				"  %s%s: EXCLUDE constraints have no SQL Server equivalent — the overlap they prevent "+
					"becomes a race the application cannot win", at(t.Pos), t.Name))
		}
		for _, c := range t.Columns {
			if c.Type.Enum {
				if _, ok := enums[c.Type.Name]; !ok {
					problems = append(problems, fmt.Sprintf(
						"  %s%s.%s: enum %s is not declared in the schema",
						at(colPos(t, c)), t.Name, c.Name, c.Type.Name))
				}
				continue
			}
			if _, err := TypeSQL(t.Name, c); err != nil {
				problems = append(problems, "  "+at(colPos(t, c))+err.Error())
			}
			if why := checkDefault(c); why != "" {
				problems = append(problems, fmt.Sprintf("  %s%s.%s %s",
					at(colPos(t, c)), t.Name, c.Name, why))
			}
		}
		if len(t.PrimaryKey) == 0 {
			problems = append(problems, fmt.Sprintf(
				"  %s%s: a SQL Server table with no primary key is a heap, which nothing can "+
					"reference and which no filtered index can be built against", at(t.Pos), t.Name))
		}
		for _, ix := range t.Indexes {
			checkIndex(t, ix, &problems)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("this model does not port to SQL Server:\n%s", strings.Join(problems, "\n"))
}

// checkDefault refuses a default that has no faithful SQL Server spelling.
//
// uuidv7() is the one. NEWID() is a version 4 uuid — random, not time-ordered —
// and NEWSEQUENTIALID() is ordered but is neither a v7 nor safe to expose,
// since it derives from the server's MAC address. Substituting either would
// keep the column type and lose the property the model asked for, which is
// index locality: v7 keys are appended, v4 keys are scattered, and on a
// clustered primary key that is the difference between an append and a page
// split per insert.
func checkDefault(c *schema.Column) string {
	if strings.EqualFold(strings.TrimSpace(c.Default), "uuidv7()") {
		return "defaults to uuidv7(); SQL Server's NEWID() is a version 4 uuid and " +
			"NEWSEQUENTIALID() derives from the server's MAC address, so neither is a " +
			"time-ordered uuid — declare storm.GenRandomUUID() for a portable key, or " +
			"assign the key in application code"
	}
	return ""
}

// CreateIndex renders a CREATE INDEX.
//
// Filtered and covering indexes both exist here, so what myddl has to refuse or
// widen is emitted directly. The translation that IS made is the nullable
// unique:
//
// PostgreSQL's UNIQUE treats NULLs as distinct, so a nullable unique column
// accepts any number of NULL rows. SQL Server's treats them as equal and
// accepts exactly one. That is an ANSWER difference — the second NULL row is
// refused — so it cannot be left alone, and it cannot be widened away either.
// A filtered index is the exact translation: `WHERE col IS NOT NULL` indexes
// only the rows PostgreSQL's index would have constrained, so both servers
// accept and refuse the same rows.
//
// NullsNotDistinct — a PostgreSQL model asking for NULLs to CONFLICT — is
// therefore SQL Server's plain unique index, with no filter. myddl has to
// refuse it; here it is the default behaviour.
func CreateIndex(t *schema.Table, ix *schema.Index) string {
	var b strings.Builder
	b.WriteString("CREATE ")
	if ix.Unique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX " + Ident(indexName(t, ix)) + " ON " + Ident(t.Name) + " (")
	parts := make([]string, len(ix.Columns))
	for i, c := range ix.Columns {
		if c.Expr {
			// A SQL Server index key is a column, never an expression: the
			// expression has to be a PERSISTED computed column first. Check
			// refuses this, so what is emitted here is only ever reached for a
			// model that already passed.
			parts[i] = "(" + c.Name + ")"
		} else {
			parts[i] = Ident(c.Name)
		}
		if c.Desc {
			parts[i] += " DESC"
		}
	}
	b.WriteString(strings.Join(parts, ", ") + ")")
	if len(ix.Include) > 0 {
		b.WriteString(" INCLUDE (" + identList(ix.Include) + ")")
	}
	switch {
	case ix.Where != "":
		b.WriteString(" WHERE " + ix.Where)
	case ix.Unique && !ix.NullsNotDistinct:
		if w := nullableFilter(t, ix); w != "" {
			b.WriteString(" WHERE " + w)
		}
	}
	b.WriteString(";")
	return b.String()
}

// nullableFilter is the predicate that makes a unique index treat NULLs the way
// PostgreSQL does, or "" when no key column is nullable and the two agree
// already.
func nullableFilter(t *schema.Table, ix *schema.Index) string {
	var parts []string
	for _, c := range ix.Columns {
		if c.Expr {
			continue
		}
		if col := t.Column(c.Name); col != nil && !col.NotNull {
			parts = append(parts, Ident(c.Name)+" IS NOT NULL")
		}
	}
	return strings.Join(parts, " AND ")
}

// checkIndex is the index half of Check.
func checkIndex(t *schema.Table, ix *schema.Index, problems *[]string) {
	add := func(format string, a ...any) {
		*problems = append(*problems, fmt.Sprintf("  %s%s: index %s "+format,
			append([]any{at(indexPos(t, ix)), t.Name, ix.Name}, a...)...))
	}
	switch ix.Method {
	case "", "btree":
	case "hash":
		add("is a hash index; SQL Server has those only on memory-optimized tables, so a btree is what a disk table gets")
	case "gin":
		add("is a gin index, which SQL Server lacks — full-text search is a separate catalog and CONTAINS(); JSON is indexed through a PERSISTED computed column")
	case "gist", "spgist":
		add("is a %s index, which SQL Server lacks — its spatial index over a geography or geometry column is the nearest thing", ix.Method)
	case "brin":
		add("is a brin index, which SQL Server lacks — a columnstore index is the closest in spirit, at a different shape")
	case "fulltext":
		add("is a fulltext index; SQL Server's needs a CREATE FULLTEXT CATALOG and a key index, which storm does not generate")
	default:
		add("uses access method %q, which SQL Server lacks", ix.Method)
	}
	for _, p := range ix.With {
		add("sets storage parameter %s, which SQL Server has no equivalent for", p.Name)
	}
	for _, c := range ix.Columns {
		if c.OpClass != "" {
			add("gives %s the operator class %s; SQL Server has no operator classes", c.Name, c.OpClass)
		}
		if c.NullsFirst || c.NullsLast {
			add("places NULLs on %s; SQL Server sorts NULLs first in ascending order and has no clause to change it", c.Name)
		}
		if c.Prefix > 0 {
			add("indexes %d characters of %s; SQL Server has no prefix keys — give the column a Size instead", c.Prefix, c.Name)
		}
		if c.Expr {
			add("keys on the expression %s; a SQL Server index key is a column, so the expression has to be a PERSISTED computed column first", c.Name)
			continue
		}
		// A MAX column cannot be an index KEY — the limit is 1700 bytes for a
		// nonclustered index and MAX has no bound. It CAN be an INCLUDE column,
		// which is the fix worth naming because SQL Server, unlike MySQL, has
		// one.
		if col := t.Column(c.Name); col != nil && isMax(col) {
			add("indexes %s, an %s column, which cannot be an index key — give it a Size, "+
				"or carry it as an INCLUDE column instead", c.Name, maxName(col))
		}
	}
}

// isMax reports whether a column becomes an unbounded NVARCHAR or VARBINARY.
func isMax(c *schema.Column) bool {
	switch c.Type.Name {
	case schema.TypeText, schema.TypeBytea, schema.TypeJSON, schema.TypeJSONB:
		return true
	case schema.TypeVarchar:
		return c.Type.Size == 0
	}
	return false
}

func maxName(c *schema.Column) string {
	if c.Type.Name == schema.TypeBytea {
		return "VARBINARY(MAX)"
	}
	return "NVARCHAR(MAX)"
}

// AddForeignKey renders an ALTER TABLE ... ADD CONSTRAINT.
//
// RESTRICT becomes NO ACTION. SQL Server has no RESTRICT keyword, and NO ACTION
// is what it means: the delete is refused if a child row references the parent.
// The difference PostgreSQL draws between them — RESTRICT checks immediately,
// NO ACTION defers to the end of the statement — is not observable through a
// constraint storm generates, which is never DEFERRABLE.
func AddForeignKey(t *schema.Table, fk *schema.ForeignKey) string {
	var b strings.Builder
	b.WriteString("ALTER TABLE " + Ident(t.Name) + " ADD CONSTRAINT " + Ident(fk.Name))
	b.WriteString(" FOREIGN KEY (" + identList(fk.Columns) + ")")
	b.WriteString(" REFERENCES " + Ident(fk.RefTable) + " (" + identList(fk.RefColumns) + ")")
	if a := fkAction(fk.OnDelete); a != "" {
		b.WriteString(" ON DELETE " + a)
	}
	if a := fkAction(fk.OnUpdate); a != "" {
		b.WriteString(" ON UPDATE " + a)
	}
	b.WriteString(";")
	return b.String()
}

func fkAction(a schema.Action) string {
	if a == schema.Restrict {
		return "NO ACTION"
	}
	return string(a)
}

// mssqlDefault translates the defaults storm generates.
func mssqlDefault(c *schema.Column) string {
	switch strings.ToLower(strings.TrimSpace(c.Default)) {
	case "now()", "current_timestamp":
		return "SYSDATETIMEOFFSET()"
	case "gen_random_uuid()":
		// Server-side, and a real version 4 uuid. This is the difference from
		// MySQL that lets the key stay the database's job here: with a DEFAULT
		// and an OUTPUT clause, an insert that names no key still comes back
		// carrying one.
		return "NEWID()"
	case "uuidv7()":
		// Refused by Check with the reasoning; nothing is emitted so that a
		// model reaching here without the check does not silently get v4.
		return ""
	}
	return c.Default
}

// Ident quotes an identifier with brackets, which is SQL Server's spelling —
// double quotes work only under QUOTED_IDENTIFIER ON, and that is a
// per-connection setting a library may not assume.
func Ident(s string) string { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }

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
	parts := make([]string, 0, len(ix.Columns)+2)
	parts = append(parts, "ix", t.Name)
	for _, c := range ix.Columns {
		parts = append(parts, c.Name)
	}
	return strings.Join(parts, "_")
}

func unsupported(table string, c *schema.Column, why, fix string) error {
	s := fmt.Sprintf("%s.%s is %s: %s", table, c.Name, c.Type.SQL(), why)
	if fix != "" {
		s += " — " + fix
	}
	return fmt.Errorf("%s", s)
}
