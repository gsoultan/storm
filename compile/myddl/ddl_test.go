package myddl_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/myddl"
	"github.com/gsoultan/storm/schema"
)

func col(name, ty string) *schema.Column {
	return &schema.Column{Name: name, Type: schema.Type{Name: ty}, NotNull: true}
}

// The types that DO cross, and what they become. Every row is a decision, not
// a default — DATETIME(6) rather than TIMESTAMP, BINARY(16) rather than
// CHAR(36) — and a change to one should have to argue with this.
func TestTypeMapping(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{schema.TypeBool, "TINYINT(1)"},
		{schema.TypeInt2, "SMALLINT"},
		{schema.TypeInt4, "INT"},
		{schema.TypeInt8, "BIGINT"},
		{schema.TypeFloat4, "FLOAT"},
		{schema.TypeFloat8, "DOUBLE"},
		{schema.TypeText, "LONGTEXT"},
		{schema.TypeBytea, "LONGBLOB"},
		{schema.TypeUUID, "BINARY(16)"},
		{schema.TypeTimestamptz, "DATETIME(6)"},
		{schema.TypeDate, "DATE"},
		{schema.TypeTime, "TIME(6)"},
		{schema.TypeJSONB, "JSON"},
	} {
		got, err := myddl.TypeSQL("t", col("x", c.in))
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s → %s, want %s", c.in, got, c.want)
		}
	}
}

// The point of the package. Each of these has no MySQL equivalent, and finding
// that out as a CREATE TABLE failing on a customer's server is the failure this
// replaces.
func TestUnportableTypesAreNamedWithAFix(t *testing.T) {
	for _, c := range []struct{ ty, mentions string }{
		{schema.TypeInterval, "INTERVAL"},
		{schema.TypeInet, "network address"},
		{schema.TypeTSVector, "FULLTEXT"},
		{schema.TypeTstzRange, "range types"},
		{schema.TypeMacaddr, "macaddr"},
	} {
		_, err := myddl.TypeSQL("bookings", col("v", c.ty))
		if err == nil {
			t.Errorf("%s was accepted; MySQL has no such type", c.ty)
			continue
		}
		if !strings.Contains(err.Error(), c.mentions) {
			t.Errorf("%s: error does not mention %q: %v", c.ty, c.mentions, err)
		}
		// Every one must name the column, or it is not actionable.
		if !strings.Contains(err.Error(), "bookings.v") {
			t.Errorf("%s: error does not name the column: %v", c.ty, err)
		}
	}
}

// An array is the one people are most surprised by.
func TestArraysDoNotCross(t *testing.T) {
	c := &schema.Column{Name: "tags", Type: schema.Type{Name: schema.TypeText, Array: true}}
	_, err := myddl.TypeSQL("posts", c)
	if err == nil {
		t.Fatal("a text[] was accepted")
	}
	if !strings.Contains(err.Error(), "no array type") {
		t.Errorf("got %v", err)
	}
}

// An unbounded numeric is the dangerous one: MySQL's unspecified DECIMAL means
// DECIMAL(10,0), which truncates every fraction. Refused rather than guessed —
// an accounting column that quietly loses its cents is the worst portability
// failure there is.
func TestUnboundedNumericIsRefused(t *testing.T) {
	_, err := myddl.TypeSQL("payments", col("amount", schema.TypeNumeric))
	if err == nil {
		t.Fatal("an unbounded numeric was accepted; MySQL would truncate every fraction")
	}
	if !strings.Contains(err.Error(), "DECIMAL(10,0)") {
		t.Errorf("the error does not explain what MySQL would do: %v", err)
	}
	// A declared precision is fine.
	got, err := myddl.TypeSQL("payments", &schema.Column{
		Name: "amount", Type: schema.Type{Name: schema.TypeNumeric, Precision: 19, Scale: 4}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "DECIMAL(19,4)" {
		t.Errorf("got %s", got)
	}
}

// Check reports EVERY problem at once. One deploy per problem is the failure
// mode this replaces.
func TestCheckReportsEveryProblem(t *testing.T) {
	s := &schema.Schema{Tables: []*schema.Table{{
		Name:       "t",
		PrimaryKey: []string{"id"},
		Columns: []*schema.Column{
			col("id", schema.TypeUUID),
			col("dur", schema.TypeInterval),
			col("ip", schema.TypeInet),
			col("amount", schema.TypeNumeric),
		},
	}}}
	err := myddl.Check(s)
	if err == nil {
		t.Fatal("Check accepted an unportable schema")
	}
	for _, want := range []string{"dur", "ip", "amount"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Check stopped early; %q missing from:\n%v", want, err)
		}
	}
}

// Exclusion constraints have no MySQL equivalent, and the consequence is worth
// spelling out: the overlap they prevent becomes a race.
func TestExclusionConstraintsAreCalledOut(t *testing.T) {
	s := &schema.Schema{Tables: []*schema.Table{{
		Name: "bookings", PrimaryKey: []string{"id"},
		Columns:  []*schema.Column{col("id", schema.TypeUUID)},
		Excludes: []*schema.Exclude{{Name: "ex"}},
	}}}
	err := myddl.Check(s)
	if err == nil {
		t.Fatal("an exclusion constraint was accepted")
	}
	if !strings.Contains(err.Error(), "race") {
		t.Errorf("the error does not say what is lost: %v", err)
	}
}

// A table with no primary key: InnoDB invents a hidden one that nothing can
// reference.
func TestMissingPrimaryKeyIsReported(t *testing.T) {
	s := &schema.Schema{Tables: []*schema.Table{{
		Name: "t", Columns: []*schema.Column{col("x", schema.TypeInt4)},
	}}}
	if err := myddl.Check(s); err == nil || !strings.Contains(err.Error(), "InnoDB") {
		t.Errorf("got %v", err)
	}
}

// Identifiers are backticked, not double-quoted.
func TestIdentifierQuoting(t *testing.T) {
	if got := myddl.Ident("order"); got != "`order`" {
		t.Errorf("got %s", got)
	}
	if got := myddl.Ident("we`ird"); got != "`we``ird`" {
		t.Errorf("got %s", got)
	}
}

func TestEnumBecomesNative(t *testing.T) {
	got := myddl.TypeEnum(&schema.Enum{Name: "status", Labels: []string{"a", "b"}})
	if got != "ENUM('a', 'b')" {
		t.Errorf("got %s", got)
	}
}

func lobTable() *schema.Table {
	return &schema.Table{Name: "docs", PrimaryKey: []string{"id"}, Columns: []*schema.Column{
		col("id", schema.TypeUUID),
		col("body", schema.TypeText),
		{Name: "slug", Type: schema.Type{Name: schema.TypeVarchar, Size: 200}, NotNull: true},
		col("score", schema.TypeInt4),
	}}
}

// MySQL's own index forms: a prefix length, a FULLTEXT index, a HASH, a
// functional key part, and an INVISIBLE index.
func TestCreateIndex_MySQLForms(t *testing.T) {
	for _, c := range []struct {
		name string
		ix   *schema.Index
		want string
	}{
		{"prefix", &schema.Index{Name: "i", Columns: []schema.IndexColumn{{Name: "body", Prefix: 191}}},
			"CREATE INDEX `i` ON `docs` (`body`(191));"},
		{"fulltext", &schema.Index{Name: "i", Method: "fulltext", Columns: []schema.IndexColumn{{Name: "body"}}},
			"CREATE FULLTEXT INDEX `i` ON `docs` (`body`);"},
		{"hash", &schema.Index{Name: "i", Method: "hash", Columns: []schema.IndexColumn{{Name: "slug"}}},
			"CREATE INDEX `i` ON `docs` (`slug`) USING HASH;"},
		{"functional key part", &schema.Index{Name: "i", Columns: []schema.IndexColumn{{Name: "lower(slug)", Expr: true}}},
			"CREATE INDEX `i` ON `docs` ((lower(slug)));"},
		{"invisible unique desc", &schema.Index{Name: "i", Unique: true, Invisible: true,
			Columns: []schema.IndexColumn{{Name: "slug", Desc: true}}},
			"CREATE UNIQUE INDEX `i` ON `docs` (`slug` DESC) INVISIBLE;"},
	} {
		if got := myddl.CreateIndex(lobTable(), c.ix); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// What PostgreSQL says that MySQL cannot do, and the one thing MySQL demands
// that PostgreSQL does not: a key length on a TEXT column. Each is refused
// rather than dropped, because an index without its WHERE is a different
// index and a TEXT key without a length is a CREATE that fails.
func TestCheck_RefusesWhatMySQLLacksAndDemandsAKeyLength(t *testing.T) {
	for _, c := range []struct {
		name string
		ix   *schema.Index
		want string
	}{
		{"gin", &schema.Index{Name: "i", Method: "gin", Columns: []schema.IndexColumn{{Name: "slug"}}}, "gin"},
		{"brin", &schema.Index{Name: "i", Method: "brin", Columns: []schema.IndexColumn{{Name: "score"}}}, "brin"},
		// A partial UNIQUE index only. Widening one changes ANSWERS — rows the
		// predicate excluded could coexist and now conflict — which is the
		// soft-delete case. The non-unique form is widened instead, and
		// TestAPartialNonUniqueIndexIsWidenedRatherThanRefused says so.
		{"partial unique", &schema.Index{Name: "i", Unique: true, Where: "score > 0",
			Columns: []schema.IndexColumn{{Name: "score"}}}, "partial unique"},
		{"include", &schema.Index{Name: "i", Include: []string{"score"}, Columns: []schema.IndexColumn{{Name: "slug"}}}, "INCLUDE"},
		{"nulls not distinct", &schema.Index{Name: "i", Unique: true, NullsNotDistinct: true, Columns: []schema.IndexColumn{{Name: "slug"}}}, "NULLS NOT DISTINCT"},
		{"opclass", &schema.Index{Name: "i", Columns: []schema.IndexColumn{{Name: "slug", OpClass: "text_pattern_ops"}}}, "operator class"},
		{"collation", &schema.Index{Name: "i", Columns: []schema.IndexColumn{{Name: "slug", Collate: "C"}}}, "collat"},
		{"nulls placement", &schema.Index{Name: "i", Columns: []schema.IndexColumn{{Name: "score", Desc: true, NullsLast: true}}}, "NULLs"},
		{"storage parameter", &schema.Index{Name: "i", With: []schema.StorageParam{{Name: "fillfactor", Value: "70"}}, Columns: []schema.IndexColumn{{Name: "slug"}}}, "fillfactor"},
		{"text key without a length", &schema.Index{Name: "i", Columns: []schema.IndexColumn{{Name: "body"}}}, "key length"},
	} {
		s := &schema.Schema{Tables: []*schema.Table{lobTable()}}
		s.Tables[0].Indexes = []*schema.Index{c.ix}
		err := myddl.Check(s)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error does not mention %q:\n%v", c.name, c.want, err)
		}
	}
	// And the forms MySQL does have pass: a prefixed TEXT key, a FULLTEXT over
	// TEXT, a VARCHAR key with no length.
	s := &schema.Schema{Tables: []*schema.Table{lobTable()}}
	s.Tables[0].Indexes = []*schema.Index{
		{Name: "a", Columns: []schema.IndexColumn{{Name: "body", Prefix: 191}}},
		{Name: "b", Method: "fulltext", Columns: []schema.IndexColumn{{Name: "body"}}},
		{Name: "c", Columns: []schema.IndexColumn{{Name: "slug"}}, Invisible: true},
	}
	if err := myddl.Check(s); err != nil {
		t.Fatalf("MySQL's own forms were refused: %v", err)
	}
}

// The bare Create/CreateTable/ColumnDef mean MySQL. They exist so that adding
// the Target did not change what every existing caller gets — and a wrapper
// nothing calls is a wrapper that can quietly point at the wrong target.
func TestTheBareFormsMeanMySQL(t *testing.T) {
	s := &schema.Schema{Tables: []*schema.Table{{
		Name:       "users",
		Columns:    []*schema.Column{col("id", schema.TypeUUID), col("email", schema.TypeVarchar)},
		PrimaryKey: []string{"id"},
	}}}
	s.Tables[0].Columns[1].Type.Size = 320

	bare, err := myddl.Create(s)
	if err != nil {
		t.Fatal(err)
	}
	forMySQL, err := myddl.CreateFor(s, myddl.MySQL)
	if err != nil {
		t.Fatal(err)
	}
	if bare != forMySQL {
		t.Errorf("Create is not CreateFor(MySQL):\n%s\n%s", bare, forMySQL)
	}

	tbl, err := myddl.CreateTable(s.Tables[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if tblFor, _ := myddl.CreateTableFor(s.Tables[0], myddl.MySQL, nil); tbl != tblFor {
		t.Error("CreateTable is not CreateTableFor(MySQL)")
	}
	cd, err := myddl.ColumnDef("users", s.Tables[0].Columns[1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if cdFor, _ := myddl.ColumnDefFor("users", s.Tables[0].Columns[1], myddl.MySQL, nil); cd != cdFor {
		t.Error("ColumnDef is not ColumnDefFor(MySQL)")
	}
	if !strings.Contains(cd, "`email` VARCHAR(320) NOT NULL") {
		t.Errorf("ColumnDef = %q", cd)
	}
}

// A foreign key is a separate ALTER rather than an inline REFERENCES, because
// a table can reference one created after it and storm emits tables in model
// order. The referential actions have to survive the crossing.
func TestForeignKeyIsAnAlterWithItsActions(t *testing.T) {
	tbl := &schema.Table{Name: "orders"}
	fk := &schema.ForeignKey{
		Name:       "fk_orders_customer",
		Columns:    []string{"customer_id"},
		RefTable:   "customers",
		RefColumns: []string{"id"},
		OnDelete:   schema.Cascade,
		OnUpdate:   schema.Restrict,
	}
	got := myddl.AddForeignKey(tbl, fk)
	for _, want := range []string{
		"ALTER TABLE `orders` ADD CONSTRAINT `fk_orders_customer`",
		"FOREIGN KEY (`customer_id`)",
		"REFERENCES `customers` (`id`)",
		"ON DELETE CASCADE",
		"ON UPDATE RESTRICT",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, ";") {
		t.Errorf("unterminated: %s", got)
	}

	// No actions declared means no clauses, not an empty ON DELETE.
	plain := myddl.AddForeignKey(tbl, &schema.ForeignKey{
		Name: "fk_plain", Columns: []string{"a"}, RefTable: "b", RefColumns: []string{"id"},
	})
	if strings.Contains(plain, "ON DELETE") || strings.Contains(plain, "ON UPDATE") {
		t.Errorf("an undeclared action was emitted:\n%s", plain)
	}
}

// A partial NON-UNIQUE index is widened rather than refused.
//
// Dropping the predicate costs storage and a little scan time; it changes no
// answer, because the rows it adds are rows the query does not ask for. The
// unique form is refused, because there widening turns rows PostgreSQL accepts
// into a constraint violation.
//
// This is not a hypothetical split. A polymorphic arc generates one partial
// index per variant — WHERE <variant>_id IS NOT NULL — so refusing them refuses
// storm.OneOfN on this target entirely.
func TestAPartialNonUniqueIndexIsWidenedRatherThanRefused(t *testing.T) {
	tbl := &schema.Table{
		Name:       "attachments",
		Columns:    []*schema.Column{col("id", schema.TypeUUID), col("post_id", schema.TypeUUID)},
		PrimaryKey: []string{"id"},
		Indexes: []*schema.Index{{
			Name:    "ix_attachments_post_id",
			Where:   `"post_id" IS NOT NULL`,
			Columns: []schema.IndexColumn{{Name: "post_id"}},
		}},
	}
	s := &schema.Schema{Tables: []*schema.Table{tbl}}
	if err := myddl.Check(s); err != nil {
		t.Fatalf("a partial non-unique index was refused: %v", err)
	}
	got := myddl.CreateIndex(tbl, tbl.Indexes[0])
	if strings.Contains(got, "WHERE") {
		t.Errorf("the predicate survived into MySQL DDL, which has no partial index:\n%s", got)
	}
	if !strings.Contains(got, "`post_id`") {
		t.Errorf("the index lost its column:\n%s", got)
	}

	// ...and the unique form is still refused, because that one changes answers.
	tbl.Indexes[0].Unique = true
	err := myddl.Check(s)
	if err == nil {
		t.Fatal("a partial UNIQUE index was accepted")
	}
	if !strings.Contains(err.Error(), "partial unique") {
		t.Errorf("the refusal does not name what it refused: %v", err)
	}
}

// An arc's exactly-one CHECK, which storm writes rather than the model.
//
// PostgreSQL's spelling casts each comparison with `::int`, which parses
// nowhere else — MySQL rejected the whole CREATE TABLE. Here a boolean already
// is 1 or 0 in arithmetic, so the cast simply goes away. A check the MODEL
// declared is passed through untouched, because rewriting somebody's
// expression is guesswork.
func TestAnArcCheckIsRespelledAndADeclaredOneIsNot(t *testing.T) {
	tbl := &schema.Table{
		Name: "attachments",
		Columns: []*schema.Column{
			col("id", schema.TypeUUID), col("post_id", schema.TypeUUID),
			col("user_id", schema.TypeUUID),
		},
		PrimaryKey: []string{"id"},
		Checks: []*schema.Check{
			{
				Name: "ck_attachments_subject",
				Expr: `("post_id" IS NOT NULL)::int + ("user_id" IS NOT NULL)::int = 1`,
				Arc:  []string{"post_id", "user_id"},
			},
			{Name: "ck_attachments_named", Expr: `char_length("id") > 0`},
		},
	}
	got, err := myddl.CreateTable(tbl, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The arc's, respelled.
	if strings.Contains(got, "::int") {
		t.Errorf("a PostgreSQL cast survived into MySQL DDL:\n%s", got)
	}
	want := "CHECK ((`post_id` IS NOT NULL) + (`user_id` IS NOT NULL) = 1)"
	if !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
	// The model's, untouched.
	if !strings.Contains(got, `CHECK (char_length("id") > 0)`) {
		t.Errorf("a declared check was rewritten:\n%s", got)
	}

	// At-most-one rather than exactly-one.
	tbl.Checks[0].ArcOptional = true
	got, err = myddl.CreateTable(tbl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "IS NOT NULL) <= 1)") {
		t.Errorf("an optional arc is not at-most-one:\n%s", got)
	}
}

// A refusal names the LINE that declared the thing it refuses.
//
// v1's definition says an unsupported construct "fails generation, naming the
// target and the source line". It named the target, the table and the column,
// and never a line — which an adopter with forty models turns into a grep.
func TestARefusalNamesTheDeclarationsLine(t *testing.T) {
	arr := col("tags", schema.TypeText)
	arr.Type.Array = true
	arr.Pos = "model/model.go:12:2"
	tbl := &schema.Table{
		Name:       "docs",
		Pos:        "model/model.go:8:6",
		Columns:    []*schema.Column{col("id", schema.TypeUUID), arr},
		PrimaryKey: []string{"id"},
	}
	s := &schema.Schema{Tables: []*schema.Table{tbl}}

	err := myddl.Check(s)
	if err == nil {
		t.Fatal("an array column ported to MySQL")
	}
	if !strings.Contains(err.Error(), "model/model.go:12:2") {
		t.Errorf("the refusal does not name the FIELD's line:\n%v", err)
	}
	// The rest of the message is unchanged: the line is a prefix, not a
	// replacement for saying what is wrong and what to do.
	for _, want := range []string{"docs.tags", "no array type", "normalise it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lost %q:\n%v", want, err)
		}
	}

	// An index problem names its first column's line, because an index is
	// declared in a Schema method and the column is the nearest thing a reader
	// can act on.
	tbl.Columns = []*schema.Column{col("id", schema.TypeUUID), col("body", schema.TypeText)}
	tbl.Columns[1].Pos = "model/model.go:11:2"
	tbl.Indexes = []*schema.Index{{
		Name: "ix_docs_body", Columns: []schema.IndexColumn{{Name: "body"}},
	}}
	err = myddl.Check(s)
	if err == nil {
		t.Fatal("an unbounded text column was indexed without a key length")
	}
	if !strings.Contains(err.Error(), "model/model.go:11:2") {
		t.Errorf("the index refusal does not name a line:\n%v", err)
	}
}

// A schema with no positions — from introspection, or from a Build the tool did
// not annotate — reads exactly as it did before. The line is an addition, not a
// requirement.
func TestARefusalWithoutAPositionIsUnchanged(t *testing.T) {
	arr := col("tags", schema.TypeText)
	arr.Type.Array = true
	s := &schema.Schema{Tables: []*schema.Table{{
		Name:       "docs",
		Columns:    []*schema.Column{col("id", schema.TypeUUID), arr},
		PrimaryKey: []string{"id"},
	}}}
	err := myddl.Check(s)
	if err == nil {
		t.Fatal("an array column ported to MySQL")
	}
	if !strings.Contains(err.Error(), "  docs.tags is") {
		t.Errorf("a message with no position is not the bare form:\n%v", err)
	}
	if strings.Contains(err.Error(), "::") {
		t.Errorf("an empty position left a stray separator:\n%v", err)
	}
}

// An enum column becomes a NATIVE MySQL ENUM, which names its values in the
// column definition — so the labels are needed at render time, and for a long
// time nothing supplied them: Check accepted a model with an enum and Create
// then refused it.
func TestEnumColumnRendersRatherThanRefusing(t *testing.T) {
	e := &schema.Enum{Name: "order_status", Labels: []string{"new", "paid"}}
	c := &schema.Column{Name: "status",
		Type: schema.Type{Name: "order_status", Enum: true}, NotNull: true}
	tb := &schema.Table{Name: "orders", Columns: []*schema.Column{
		{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true}, c,
	}, PrimaryKey: []string{"id"}}
	s := &schema.Schema{Tables: []*schema.Table{tb}, Enums: []*schema.Enum{e}}

	if err := myddl.Check(s); err != nil {
		t.Fatalf("Check refused a model with a declared enum: %v", err)
	}
	got, err := myddl.Create(s)
	if err != nil {
		t.Fatalf("Check accepted this model and Create refused it: %v", err)
	}
	if !strings.Contains(got, "`status` ENUM('new', 'paid') NOT NULL") {
		t.Errorf("the column is not a native ENUM:\n%s", got)
	}
}
