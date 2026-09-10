package mysql_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/mysql"
	"github.com/gsoultan/storm/compile/pgsql"
)

// The three differences that are not spellings. Each of these was emitted as
// PostgreSQL until compile/mysql existed, and each is an Error 1064 on MySQL
// 8.4.11 rather than a wrong answer — which is the only mercy in it.

func TestIdentifiersAreBackticked(t *testing.T) {
	if got := mysql.Ident("my_users"); got != "`my_users`" {
		t.Fatalf("Ident = %s", got)
	}
	// Not a style choice. Default sql_mode has no ANSI_QUOTES, so a
	// double-quoted name is a string LITERAL: `SELECT "id" FROM "users"` does
	// not fail, it selects the constant "id". A library cannot assume someone
	// else's sql_mode.
	if strings.Contains(mysql.SelectPrefix("t", []string{"c"}), `"`) {
		t.Error("a double quote reached MySQL SQL")
	}
	// A backtick inside an identifier is doubled, not dropped.
	if got := mysql.Ident("we`ird"); got != "`we``ird`" {
		t.Fatalf("Ident does not escape backticks: %s", got)
	}
}

func TestPlaceholderIsBare(t *testing.T) {
	if mysql.Placeholder != "?" {
		t.Fatalf("Placeholder = %q", mysql.Placeholder)
	}
	a, _, _ := mysql.Frag("Eq", mysql.Ident("email"))
	if strings.Contains(a, "$") {
		t.Errorf("a PostgreSQL placeholder reached MySQL SQL: %s", a)
	}
}

// The operator that could have sunk M9 (ADR-0010).
//
// PostgreSQL lowers In to `= ANY($1)` — one placeholder for a whole list, so
// the statement text does not depend on the caller's data. MySQL's IN (?,?,?)
// has value-dependent arity, which makes the shape key a function of REQUEST
// DATA rather than of the program.
func TestInHasOneShapeForEveryListLength(t *testing.T) {
	a, b := mysql.InFrag(mysql.Ident("id"), "BIGINT", false)
	sql := a + b
	if strings.Count(sql, "?") != 1 {
		t.Fatalf("In binds %d placeholders; it must bind exactly one JSON document:\n%s",
			strings.Count(sql, "?"), sql)
	}
	if !strings.Contains(sql, "JSON_TABLE") {
		t.Errorf("In is not the JSON_TABLE lowering ADR-0010 chose:\n%s", sql)
	}
	// The typed COLUMNS declaration is what keeps an index lookup in the plan.
	// Declared JSON it compares a JSON scalar to a native value: wrong, and
	// unindexable.
	if !strings.Contains(sql, "COLUMNS (`v` BIGINT PATH '$')") {
		t.Errorf("the JSON_TABLE column is not typed to the column it matches:\n%s", sql)
	}
	// The same text for a different type, and only the type moves.
	other, _ := mysql.InFrag(mysql.Ident("id"), "VARCHAR(320)", false)
	if strings.Replace(other, "VARCHAR(320)", "BIGINT", 1) != a {
		t.Errorf("In differs by more than the column type:\n%s\n%s", a, other)
	}
}

func TestNotInNegatesWithoutChangingShape(t *testing.T) {
	in, _ := mysql.InFrag(mysql.Ident("id"), "BIGINT", false)
	notIn, _ := mysql.InFrag(mysql.Ident("id"), "BIGINT", true)
	if strings.Replace(notIn, " NOT IN (", " IN (", 1) != in {
		t.Errorf("NotIn is not In negated:\n%s\n%s", in, notIn)
	}
}

// JSON containment is a FUNCTION in MySQL, so it wraps the identifier instead
// of following it. A table of infix operators cannot express that, which is
// why Frag returns a prefix and a suffix.
func TestJSONContainmentWrapsTheIdentifier(t *testing.T) {
	a, b, ok := mysql.Frag("JSONContains", mysql.Ident("doc"))
	if !ok {
		t.Fatal("no lowering for JSONContains")
	}
	if got := a + b; got != "JSON_CONTAINS(`doc`, ?)" {
		t.Fatalf("JSONContains = %s", got)
	}
}

// PostgreSQL's array, range and network operators have no MySQL equivalent.
// compile/myddl already refuses those COLUMN types; their absence here is the
// same refusal on the read path, so the two cannot disagree.
func TestOperatorsWithNoMySQLEquivalentAreAbsent(t *testing.T) {
	for _, op := range []string{"ArrayContains", "ArrayOverlaps", "ArrayContainedBy"} {
		if _, _, ok := mysql.Frag(op, "`c`"); ok {
			t.Errorf("%s has a MySQL lowering; MySQL has no array type", op)
		}
		if mysql.Supported(op) {
			t.Errorf("Supported(%s) is true", op)
		}
	}
	// And they DO exist for PostgreSQL, so this is a real divergence rather
	// than an operator nobody implemented anywhere.
	if _, _, ok := pgsql.Frag("ArrayContains", `"c"`); !ok {
		t.Error("fixture is wrong: PostgreSQL has no ArrayContains either")
	}
}

// MySQL 8 cannot return the row it wrote, so there is no output clause to emit
// and the write path has to read back instead.
func TestNoOutputClauseIsOffered(t *testing.T) {
	ins := mysql.InsertPrefix("t")
	if strings.Contains(strings.ToUpper(ins), "RETURN") {
		t.Errorf("the insert prefix carries an output clause MySQL 8 has not: %s", ins)
	}
}
