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
