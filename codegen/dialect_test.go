package codegen_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/schema"
)

func portableSchema() *schema.Schema {
	return &schema.Schema{Tables: []*schema.Table{{
		Name: "users", GoName: "User",
		Columns: []*schema.Column{
			{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
			{Name: "n", Type: schema.Type{Name: schema.TypeInt8}, NotNull: true},
			{Name: "created_at", Type: schema.Type{Name: schema.TypeTimestamptz}, NotNull: true},
			{Name: "name", Type: schema.Type{Name: schema.TypeText}, NotNull: true},
		},
		PrimaryKey: []string{"id"},
	}}}
}

// The seam's whole purpose: a MySQL package must call the MySQL decoders. The
// two families disagree byte for byte — runtime.Int8 reads a MySQL BIGINT as a
// byte-reversed number, silently — so calling the wrong one is a wrong answer
// with no error (ADR-0007).
func TestMySQLScannersCallTheMySQLDecoders(t *testing.T) {
	src, err := codegen.File(portableSchema(), codegen.Options{
		Package: "user", Import: "github.com/gsoultan/storm",
		Table: "users", Dialect: codegen.DialectMySQL,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(src)
	for _, want := range []string{"mydec.Int8(", "mydec.DateTime("} {
		if !strings.Contains(got, want) {
			t.Errorf("the MySQL package does not call %s", want)
		}
	}
	// And must NOT reach for the PostgreSQL family's decoders.
	for _, bad := range []string{"runtime.Int8(", "runtime.Timestamptz("} {
		if strings.Contains(got, bad) {
			t.Errorf("a MySQL package calls %s, which reads these bytes backwards", bad)
		}
	}
}

// The default is PostgreSQL and the zero value means it, so no existing caller
// silently changes target.
func TestZeroDialectIsPostgres(t *testing.T) {
	if codegen.DialectPostgres != 0 {
		t.Fatal("the zero Dialect is not PostgreSQL")
	}
	src, err := codegen.File(portableSchema(), codegen.Options{
		Package: "user", Import: "github.com/gsoultan/storm", Table: "users",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "runtime.Int8(") {
		t.Error("the default dialect does not emit PostgreSQL decoders")
	}
	if strings.Contains(string(src), "mydec.") {
		t.Error("the default dialect leaked MySQL decoders")
	}
}

// A dialect that has no decoder for a type must refuse rather than emit a call
// that does not compile, or worse, one that does.
func TestMySQLRefusesTypesItCannotDecode(t *testing.T) {
	for _, ty := range []schema.Type{
		{Name: schema.TypeText, Array: true},
		{Name: schema.TypeInterval},
		{Name: schema.TypeInet},
		{Name: schema.TypeTstzRange},
	} {
		s := portableSchema()
		s.Tables[0].Columns = append(s.Tables[0].Columns,
			&schema.Column{Name: "x", Type: ty})
		_, err := codegen.File(s, codegen.Options{
			Package: "user", Import: "github.com/gsoultan/storm",
			Table: "users", Dialect: codegen.DialectMySQL,
		})
		if err == nil {
			t.Errorf("%s was accepted for MySQL, which has no such type", ty.SQL())
		}
	}
}

// The declaration-time refusals in the join and aggregate emitters. Each is a
// generated file that would not compile, or would compile and be wrong, so the
// generator has to stop rather than write it.
func TestJoinAndAggregateEmitterRefusals(t *testing.T) {
	base := func() *schema.Schema { return portableSchema() }

	t.Run("aggregate selecting nothing", func(t *testing.T) {
		s := base()
		s.Tables[0].Aggregates = []*schema.Aggregate{{Name: "Empty"}}
		if _, err := codegen.File(s, opts()); err == nil {
			t.Error("an aggregate with no outputs was generated")
		}
	})

	t.Run("aggregate over a missing column", func(t *testing.T) {
		s := base()
		s.Tables[0].Aggregates = []*schema.Aggregate{{
			Name:  "X",
			Terms: []schema.AggregateTerm{{As: "N", Expr: schema.Expr{Kind: schema.ExprAgg, Fn: "sum", Type: schema.Type{Name: "nonesuch"}}}},
		}}
		if _, err := codegen.File(s, opts()); err == nil {
			t.Error("an aggregate returning an unknown type was generated")
		}
	})

	t.Run("join selecting nothing", func(t *testing.T) {
		s := base()
		s.Tables[0].Joins = []*schema.Join{{Name: "J"}}
		if _, err := codegen.File(s, opts()); err == nil {
			t.Error("a join with no outputs was generated")
		}
	})

	t.Run("join with duplicate outputs", func(t *testing.T) {
		s := base()
		c := schema.JoinCol{As: "N", Type: schema.Type{Name: schema.TypeInt8}}
		s.Tables[0].Joins = []*schema.Join{{Name: "J", Select: []schema.JoinCol{c, c}}}
		if _, err := codegen.File(s, opts()); err == nil {
			t.Error("a join with two outputs of one name was generated")
		}
	})

	t.Run("join output with no Go type", func(t *testing.T) {
		s := base()
		s.Tables[0].Joins = []*schema.Join{{Name: "J", Select: []schema.JoinCol{
			{As: "X", Type: schema.Type{Name: "nonesuch"}}}}}
		if _, err := codegen.File(s, opts()); err == nil {
			t.Error("a join output with no Go type was generated")
		}
	})
}

// A grouped read's row type: grouping columns first, then the aggregates, with
// nullability taken from the expression rather than the column.
func TestAggregateRowTypeShape(t *testing.T) {
	s := portableSchema()
	s.Tables[0].Aggregates = []*schema.Aggregate{{
		Name: "ByName",
		By:   []schema.GroupTerm{{As: "Name", Expr: schema.Expr{Kind: schema.ExprCol, Col: "name", Type: schema.Type{Name: schema.TypeText}}}},
		Terms: []schema.AggregateTerm{
			{As: "N", Expr: schema.Expr{Kind: schema.ExprAgg, Fn: "count", Type: schema.Type{Name: schema.TypeInt8}}},
			{As: "Total", Expr: schema.Expr{Kind: schema.ExprAgg, Fn: "sum", Type: schema.Type{Name: schema.TypeInt8}, Nullable: true}},
		},
	}}
	src, err := codegen.File(s, opts())
	if err != nil {
		t.Fatal(err)
	}
	got := string(src)
	for _, want := range []string{
		"type ByNameRow struct",
		"func (q Query) AllByName(",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing from the generated aggregate: %q", want)
		}
	}
	// Field types, not their gofmt alignment: count is never NULL, sum is NULL
	// over zero rows, and typing the second as a plain int64 would make "no
	// rows" indistinguishable from "sums to zero".
	row := got[strings.Index(got, "type ByNameRow struct"):]
	row = row[:strings.Index(row, "}")]
	for _, want := range []string{"N int64", "Total runtime.Null[int64]"} {
		f := strings.Fields(want)
		if !regexpField(row, f[0], f[1]) {
			t.Errorf("ByNameRow.%s is not %s:\n%s", f[0], f[1], row)
		}
	}
}

func opts() codegen.Options {
	return codegen.Options{Package: "user", Import: "github.com/gsoultan/storm", Table: "users"}
}

// regexpField reports whether a struct body declares `name typ`, whatever the
// alignment gofmt chose.
func regexpField(body, name, typ string) bool {
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == name && f[1] == typ {
			return true
		}
	}
	return false
}
