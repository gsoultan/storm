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
		{"partial", &schema.Index{Name: "i", Where: "score > 0", Columns: []schema.IndexColumn{{Name: "score"}}}, "partial"},
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
