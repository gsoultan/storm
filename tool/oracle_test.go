package tool

// Oracle is SQL-only, and the boundary is a promise rather than an accident:
// what works must work, and what does not must say why.

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/schema"
)

func oracleModel(notNull bool) *schema.Schema {
	return &schema.Schema{Tables: []*schema.Table{{
		Name:       "orgs",
		PrimaryKey: []string{"id"},
		Columns: []*schema.Column{
			{Name: "id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
			{Name: "name", Type: schema.Type{Name: schema.TypeText}, NotNull: notNull},
		},
	}}}
}

func TestOracleIsAKnownDialect(t *testing.T) {
	for _, name := range []string{"oracle", "ora"} {
		tgt, err := parseDialect(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tgt.dialect != codegen.DialectOracle {
			t.Errorf("%s resolved to %v", name, tgt.dialect)
		}
		out, err := tgt.ddl(oracleModel(true))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(out, `CREATE TABLE "orgs"`) {
			t.Errorf("want quoted, case-preserving DDL:\n%s", out)
		}
	}
}

// The refusal an adopter meets first, and the one that is about MEANING rather
// than about what can be expressed.
func TestPortableOracleNamesTheEmptyStringRule(t *testing.T) {
	err := portable("oracle", oracleModel(false))
	if err == nil {
		t.Fatal("a nullable text column ports to Oracle, apparently")
	}
	for _, want := range []string{"orgs.name", "empty string", "NOT NULL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must contain %q:\n%v", want, err)
		}
	}
	if err := portable("oracle", oracleModel(true)); err != nil {
		t.Errorf("a NOT NULL text column must port:\n%v", err)
	}
}

func TestTheUnknownDialectMessageListsOracle(t *testing.T) {
	_, err := parseDialect("db2")
	if err == nil || !strings.Contains(err.Error(), "oracle") {
		t.Errorf("the list of known dialects must include oracle: %v", err)
	}
	if err := portable("db2", oracleModel(true)); err == nil ||
		!strings.Contains(err.Error(), "oracle") {
		t.Errorf("portable's list must include oracle too: %v", err)
	}
}
