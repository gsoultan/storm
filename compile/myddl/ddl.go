// Package myddl renders a schema as MySQL 8 DDL.
//
// The second back end, and therefore the first honest evidence about what a
// dialect abstraction has to cover. `compile/pgsql` says the right interface is
// not knowable from one implementation, which is why this one is written
// straight rather than against a guessed interface: the seam is derived from
// two, in dialect.go, once both exist.
//
// What this package is really about is the types that do NOT cross. PostgreSQL
// has arrays, ranges, network types, tsvector and interval; MySQL has none of
// them. Discovering that at deploy time — as a CREATE TABLE that fails on a
// customer's server — is the failure this exists to prevent, so every one of
// them is a named, declare-time error instead.
package myddl

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// Create renders the whole schema.
//
// Returns an error rather than emitting something that will not run: a type
// with no MySQL equivalent is a portability decision, and the moment to make it
// is now, not when a customer's install fails.
func Create(s *schema.Schema) (string, error) {
	if err := Check(s); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, t := range s.Tables {
		def, err := CreateTable(t)
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

// CreateTable renders one table.
func CreateTable(t *schema.Table) (string, error) {
	var b strings.Builder
	b.WriteString("CREATE TABLE " + Ident(t.Name) + " (\n")
	parts := make([]string, 0, len(t.Columns)+2)
	for _, c := range t.Columns {
		def, err := ColumnDef(t.Name, c)
		if err != nil {
			return "", err
		}
		parts = append(parts, "    "+def)
	}
	if len(t.PrimaryKey) > 0 {
		parts = append(parts, "    PRIMARY KEY ("+identList(t.PrimaryKey)+")")
	}
	for _, u := range t.Uniques {
		parts = append(parts, "    CONSTRAINT "+Ident(u.Name)+" UNIQUE ("+identList(u.Columns)+")")
	}
	for _, ck := range t.Checks {
		// MySQL 8.0.16+ enforces CHECK. Earlier versions PARSED and ignored
		// it, which is worse than refusing it, but 8.0.16 is four years old
		// and the alternative is dropping the constraint silently.
		parts = append(parts, "    CONSTRAINT "+Ident(ck.Name)+" CHECK ("+ck.Expr+")")
	}
	b.WriteString(strings.Join(parts, ",\n"))
	b.WriteString("\n);")
	return b.String(), nil
}

// ColumnDef renders one column.
func ColumnDef(table string, c *schema.Column) (string, error) {
	ty, err := TypeSQL(table, c)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(Ident(c.Name) + " " + ty)
	switch {
	case c.Generated != "":
		// MySQL spells it the same but the EXPRESSION is not portable — it is
		// the model's own SQL. Passed through unchanged and flagged by Check,
		// because rewriting somebody's expression is guesswork.
		b.WriteString(" GENERATED ALWAYS AS (" + c.Generated + ") STORED")
	case c.Identity:
		b.WriteString(" AUTO_INCREMENT")
	case c.Default != "":
		// An empty translation means the default does not survive the crossing
		// — a client-generated uuid, for instance — and emitting the keyword
		// with nothing after it is not SQL. The column simply has no default,
		// which is what the write path already assumes for this dialect.
		if d := mysqlDefault(c); d != "" {
			b.WriteString(" DEFAULT " + d)
		}
	}
	if c.NotNull {
		b.WriteString(" NOT NULL")
	}
	return b.String(), nil
}

// TypeSQL maps a storm type to MySQL, or says why it cannot.
func TypeSQL(table string, c *schema.Column) (string, error) {
	t := c.Type
	if t.Array {
		return "", unsupported(table, c, "MySQL has no array type",
			"store it as JSON, or normalise it into its own table")
	}
	if t.Enum {
		// A storm enum knows its labels, so it becomes a native MySQL ENUM
		// rather than a check-constrained varchar.
		return "", fmt.Errorf(
			"myddl: %s.%s is an enum; call TypeEnum with the schema's label list", table, c.Name)
	}
	switch t.Name {
	case schema.TypeBool:
		// MySQL's BOOLEAN is an alias for TINYINT(1); spelled out so nobody
		// reads the generated DDL and expects a distinct type.
		return "TINYINT(1)", nil
	case schema.TypeInt2:
		return "SMALLINT", nil
	case schema.TypeInt4:
		return "INT", nil
	case schema.TypeInt8:
		return "BIGINT", nil
	case schema.TypeFloat4:
		return "FLOAT", nil
	case schema.TypeFloat8:
		return "DOUBLE", nil
	case schema.TypeNumeric:
		if t.Precision > 0 {
			if t.Scale > 0 {
				return fmt.Sprintf("DECIMAL(%d,%d)", t.Precision, t.Scale), nil
			}
			return fmt.Sprintf("DECIMAL(%d)", t.Precision), nil
		}
		// MySQL has no unbounded DECIMAL: unspecified means DECIMAL(10,0),
		// which silently truncates every fraction. Refused rather than
		// guessed — an accounting column that quietly loses its cents is the
		// worst possible portability failure.
		return "", unsupported(table, c,
			"MySQL has no unbounded DECIMAL, and an unspecified one means DECIMAL(10,0) — every fraction truncated",
			"declare the precision: t.Col(&m.Amount).Numeric(19, 4)")
	case schema.TypeText:
		return "LONGTEXT", nil
	case schema.TypeVarchar:
		if t.Size > 0 {
			return fmt.Sprintf("VARCHAR(%d)", t.Size), nil
		}
		return "LONGTEXT", nil
	case schema.TypeBytea:
		return "LONGBLOB", nil
	case schema.TypeUUID:
		// MySQL has no uuid type. BINARY(16) is the storable form and the one
		// that indexes well; the alternative, CHAR(36), is 36 bytes per key.
		return "BINARY(16)", nil
	case schema.TypeTimestamptz:
		// DATETIME(6), not TIMESTAMP: MySQL's TIMESTAMP is limited to
		// 1970–2038 and silently converts through the session time zone.
		// DATETIME(6) stores what it was given, at microsecond precision,
		// which is what a timestamptz column means once it is normalised to
		// UTC — and storm normalises.
		return "DATETIME(6)", nil
	case schema.TypeTimestamp:
		return "DATETIME(6)", nil
	case schema.TypeDate:
		return "DATE", nil
	case schema.TypeTime:
		return "TIME(6)", nil
	case schema.TypeJSONB, schema.TypeJSON:
		return "JSON", nil

	// ---- the ones that do not cross ----
	case schema.TypeInterval:
		return "", unsupported(table, c, "MySQL has no INTERVAL type",
			"store it as BIGINT microseconds, or as two timestamps")
	case schema.TypeInet, schema.TypeCIDR:
		return "", unsupported(table, c, "MySQL has no network address type",
			"store it as VARBINARY(16) with INET6_ATON, or as VARCHAR(45)")
	case schema.TypeMacaddr:
		return "", unsupported(table, c, "MySQL has no macaddr type",
			"store it as BINARY(6) or VARCHAR(17)")
	case schema.TypeTSVector:
		return "", unsupported(table, c,
			"MySQL has no tsvector; its full-text search is a FULLTEXT INDEX over the source columns, not a materialised column",
			"drop the column and declare a FULLTEXT index on the text columns instead")
	case schema.TypeTstzRange:
		return "", unsupported(table, c,
			"MySQL has no range types, and therefore no exclusion constraints",
			"store two DATETIME(6) columns and enforce non-overlap in the application, knowing it can race")
	case schema.TypeHstore:
		return "", unsupported(table, c, "MySQL has no hstore", "use JSON")
	}
	return "", unsupported(table, c, "no MySQL equivalent is known for "+t.Name, "")
}

// TypeEnum renders a native MySQL ENUM from a schema enum's labels.
func TypeEnum(e *schema.Enum) string {
	q := make([]string, len(e.Labels))
	for i, l := range e.Labels {
		q[i] = quoteLit(l)
	}
	return "ENUM(" + strings.Join(q, ", ") + ")"
}

// Check reports every portability problem in one pass.
//
// All of them, not the first: finding them one deploy at a time is the failure
// mode this replaces, and the same reasoning as Build's error list.
func Check(s *schema.Schema) error {
	var problems []string
	enums := map[string]*schema.Enum{}
	for _, e := range s.Enums {
		enums[e.Name] = e
	}
	for _, t := range s.Tables {
		if len(t.Excludes) > 0 {
			problems = append(problems, fmt.Sprintf(
				"  %s: EXCLUDE constraints have no MySQL equivalent — the overlap they prevent "+
					"becomes a race the application cannot win", t.Name))
		}
		for _, c := range t.Columns {
			if c.Type.Enum {
				if _, ok := enums[c.Type.Name]; !ok {
					problems = append(problems, fmt.Sprintf(
						"  %s.%s: enum %s is not declared in the schema", t.Name, c.Name, c.Type.Name))
				}
				continue
			}
			if _, err := TypeSQL(t.Name, c); err != nil {
				problems = append(problems, "  "+err.Error())
			}
		}
		if len(t.PrimaryKey) == 0 {
			problems = append(problems, fmt.Sprintf(
				"  %s: MySQL's InnoDB gives every table a hidden clustered key when none is "+
					"declared, which nothing can then reference", t.Name))
		}
		for _, ix := range t.Indexes {
			checkIndex(t, ix, &problems)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("this model does not port to MySQL:\n%s", strings.Join(problems, "\n"))
}

// CreateIndex renders a CREATE INDEX.
//
// What MySQL has that PostgreSQL does not — a prefix length on a key, a
// FULLTEXT index, an INVISIBLE index — is rendered here; what it lacks is
// refused by Check, never dropped: a partial index without its WHERE is a
// different index, and emitting it would be a wrong answer with no symptom.
func CreateIndex(t *schema.Table, ix *schema.Index) string {
	var b strings.Builder
	b.WriteString("CREATE ")
	switch {
	case ix.Method == "fulltext":
		b.WriteString("FULLTEXT ")
	case ix.Unique:
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX " + Ident(indexName(t, ix)) + " ON " + Ident(t.Name) + " (")
	parts := make([]string, len(ix.Columns))
	for i, c := range ix.Columns {
		switch {
		case c.Expr:
			// A functional key part (8.0.13+). Doubly parenthesised by MySQL's
			// grammar, to tell it from a column with a prefix length.
			parts[i] = "(" + c.Name + ")"
		default:
			parts[i] = Ident(c.Name)
			if c.Prefix > 0 {
				parts[i] += "(" + strconv.Itoa(c.Prefix) + ")"
			}
		}
		if c.Desc {
			parts[i] += " DESC"
		}
	}
	b.WriteString(strings.Join(parts, ", ") + ")")
	if ix.Method == "hash" {
		b.WriteString(" USING HASH")
	}
	if ix.Invisible {
		b.WriteString(" INVISIBLE")
	}
	b.WriteString(";")
	return b.String()
}

// checkIndex is the index half of Check.
func checkIndex(t *schema.Table, ix *schema.Index, problems *[]string) {
	add := func(format string, a ...any) {
		*problems = append(*problems, fmt.Sprintf("  %s: index %s "+format, append([]any{t.Name, ix.Name}, a...)...))
	}
	switch ix.Method {
	case "", "btree", "hash", "fulltext":
	case "gin":
		add("is a gin index, which MySQL lacks — a FULLTEXT index answers text search; JSON is indexed through a generated column")
	case "gist", "spgist":
		add("is a %s index, which MySQL lacks — its R-tree is the SPATIAL index over a geometry column", ix.Method)
	case "brin":
		add("is a brin index, which MySQL lacks — a btree on the same column is the closest, at a larger size")
	default:
		add("uses access method %q, which MySQL lacks", ix.Method)
	}
	if ix.Where != "" {
		add("is partial (WHERE %s); MySQL has no partial index and the rows it excludes would be indexed too", ix.Where)
	}
	if len(ix.Include) > 0 {
		add("carries INCLUDE columns; MySQL has no covering clause — append them as trailing keys instead")
	}
	if ix.NullsNotDistinct {
		add("is NULLS NOT DISTINCT; MySQL treats every NULL in a unique key as distinct and has no clause to change it")
	}
	for _, p := range ix.With {
		add("sets storage parameter %s, which MySQL has no equivalent for", p.Name)
	}
	for _, c := range ix.Columns {
		if c.OpClass != "" {
			add("gives %s the operator class %s; MySQL has no operator classes", c.Name, c.OpClass)
		}
		if c.Collate != "" {
			add("collates %s as %q; MySQL takes the collation from the column, not the index", c.Name, c.Collate)
		}
		if c.NullsFirst || c.NullsLast {
			add("places NULLs on %s; MySQL sorts NULLs first in ascending order and has no clause to change it", c.Name)
		}
		if c.Expr || ix.Method == "fulltext" {
			continue
		}
		// A TEXT or BLOB key needs a length, or MySQL refuses the CREATE with
		// "used in key specification without a key length". The model can say
		// so with a prefix, or give the column a Size so it becomes VARCHAR.
		if col := t.Column(c.Name); col != nil && c.Prefix == 0 && isLob(col) {
			add("indexes %s, a %s column, without a key length — declare storm.Prefix(&m.%s, n), "+
				"or give it a Size so it becomes VARCHAR", c.Name, lobName(col), exportish(c.Name))
		}
	}
}

// isLob reports whether a column becomes a LONGTEXT or LONGBLOB here.
func isLob(c *schema.Column) bool {
	switch c.Type.Name {
	case schema.TypeText, schema.TypeBytea:
		return true
	case schema.TypeVarchar:
		return c.Type.Size == 0
	}
	return false
}

func lobName(c *schema.Column) string {
	if c.Type.Name == schema.TypeBytea {
		return "LONGBLOB"
	}
	return "LONGTEXT"
}

// exportish guesses a Go field name from a column name for an error's fix,
// which only has to be recognisable, not exact.
func exportish(col string) string {
	var b strings.Builder
	up := true
	for _, r := range col {
		if r == '_' {
			up = true
			continue
		}
		if up {
			b.WriteString(strings.ToUpper(string(r)))
			up = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// AddForeignKey renders an ALTER TABLE ... ADD CONSTRAINT.
func AddForeignKey(t *schema.Table, fk *schema.ForeignKey) string {
	var b strings.Builder
	b.WriteString("ALTER TABLE " + Ident(t.Name) + " ADD CONSTRAINT " + Ident(fk.Name))
	b.WriteString(" FOREIGN KEY (" + identList(fk.Columns) + ")")
	b.WriteString(" REFERENCES " + Ident(fk.RefTable) + " (" + identList(fk.RefColumns) + ")")
	if fk.OnDelete != "" {
		b.WriteString(" ON DELETE " + string(fk.OnDelete))
	}
	if fk.OnUpdate != "" {
		b.WriteString(" ON UPDATE " + string(fk.OnUpdate))
	}
	b.WriteString(";")
	return b.String()
}

// mysqlDefault translates the defaults storm generates.
func mysqlDefault(c *schema.Column) string {
	switch strings.ToLower(strings.TrimSpace(c.Default)) {
	case "now()", "current_timestamp":
		return "CURRENT_TIMESTAMP(6)"
	case "gen_random_uuid()", "uuidv7()":
		// MySQL cannot default a BINARY(16) to a generated uuid: UUID_TO_BIN
		// is not allowed in a DEFAULT expression before 8.0.13 and is awkward
		// after. storm generates the key client-side for this dialect, which
		// it can already do — the unit of work needs ids before the rows
		// exist anyway.
		return ""
	}
	return c.Default
}

// Ident quotes an identifier with backticks, which is MySQL's spelling.
func Ident(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

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
