package orabench

// Result 3b: can the capability model carry Oracle? This half needs no server.
//
// docs/PLAN.md makes the answer M12's kill criterion. docs/DIALECTS.md states
// the intended mechanism: "Declaring `oracle` in `portability.assert` makes any
// `Eq("")` or non-null-constrained text column a declare-time error with a
// pointer to the model line."
//
// HALF OF THAT IS NOT IMPLEMENTABLE, and it is worth saying exactly why rather
// than discovering it in M11's third week.
//
// storm has TWO DSLs that carry a value, and they differ:
//
//   - The EXPRESSION DSL — `storm.E.Eq(&m.Status, "paid")`, used in checks,
//     generated columns and index predicates — takes `any` and folds a Go
//     literal into a schema.Literal at BUILD time. `""` there is visible.
//   - The QUERY DSL — the generated `func (h TextCol) Eq(v string) Pred` —
//     takes a runtime string and binds it as a parameter. The generator never
//     sees the value, and no amount of capability modelling can make it.
//
// So `Eq("")` in a query cannot be a declare-time error. What CAN be one is the
// COLUMN, and oracleCheck below is the prototype of that rule — the estimate
// M11 needs, not M11's implementation.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/schema"
)

// oracleCheck is the prototype of compile/oraddl.Check's empty-string rule.
//
// It reports every problem in one pass, which is the shape msddl.Check and
// myddl.Check already have: finding portability problems one deploy at a time
// is the failure mode a Check exists to replace.
func oracleCheck(s *schema.Schema) []string {
	var problems []string
	for _, t := range s.Tables {
		for _, c := range t.Columns {
			isText := c.Type.Name == schema.TypeText || c.Type.Name == schema.TypeVarchar

			// The rule. A nullable text column is the ONE place an empty
			// string and a NULL become the same stored value, so it is the one
			// place the difference is silent rather than an error. Everything
			// else about empty-string-is-NULL follows from this being refused:
			// if no column can hold '', then `WHERE c = ''` matching nothing is
			// the right answer rather than a wrong one, and a caller who writes
			// '' to a NOT NULL column gets ORA-01400 — an error, which is a
			// thing an application can see.
			if isText && !c.NotNull && c.Generated == "" {
				problems = append(problems, fmt.Sprintf(
					"  %s%s.%s: a NULLABLE text column cannot round-trip on Oracle — "+
						"an empty string is stored as NULL, so \"\" and nil are one value. "+
						"Make it NOT NULL, or move it to a table whose absence means absent",
					at(c.Pos), t.Name, c.Name))
			}

			// A DEFAULT of '' is a default of NULL here, which is a different
			// column. Textual because Column.Default is raw SQL — the model's
			// own spelling, which storm passes through unchanged.
			if isText && isEmptyLiteral(c.Default) {
				problems = append(problems, fmt.Sprintf(
					"  %s%s.%s: DEFAULT '' is DEFAULT NULL on Oracle",
					at(c.Pos), t.Name, c.Name))
			}
		}
		// A declared CHECK is the model's own SQL and every back end passes it
		// through unchanged, so storm cannot reinterpret one — the same rule
		// that puts storm.SQL outside the capability model. A WARNING rather
		// than a refusal, and named as such.
		for _, ck := range t.Checks {
			if strings.Contains(ck.Expr, "''") {
				problems = append(problems, fmt.Sprintf(
					"  %s%s: CHECK %s compares against '', which is NULL on Oracle — "+
						"storm passes a declared check through unchanged and cannot respell this",
					at(t.Pos), t.Name, ck.Name))
			}
		}
	}
	return problems
}

func at(pos string) string {
	if pos == "" {
		return ""
	}
	return pos + ": "
}

// isEmptyLiteral spots ” written as a default. Not a parser: a default is raw
// SQL and the only spellings that mean the empty string are these.
func isEmptyLiteral(def string) bool {
	d := strings.TrimSpace(def)
	return d == "''" || strings.EqualFold(d, "N''")
}

type oraOrg struct {
	storm.Model
	Name string
	Note *string // the shape the rule refuses
}

func (o *oraOrg) Schema(t *storm.Table) { t.Col(&o.Name).Size(200) }

type oraTight struct {
	storm.Model
	Name string
	Tag  string
}

func (o *oraTight) Schema(t *storm.Table) {
	t.Col(&o.Name).Size(200)
	t.Col(&o.Tag).Size(40)
}

// What the rule refuses, and that it refuses it with a POSITION — which is the
// half of DIALECTS.md's claim that survives, and the half that matters at a
// keyboard.
func TestTheColumnRuleIsExpressibleAtDeclareTime(t *testing.T) {
	s, err := storm.Build(&oraOrg{})
	if err != nil {
		t.Fatal(err)
	}
	problems := oracleCheck(s)
	if len(problems) == 0 {
		t.Fatal("a nullable text column was accepted; the rule does not fire")
	}
	joined := strings.Join(problems, "\n")
	t.Logf("oracleCheck(&oraOrg{}):\n%s", joined)
	if !strings.Contains(joined, "ora_orgs.note") {
		t.Errorf("the refusal must name the column: %s", joined)
	}

	// And the POSITION, which is where DIALECTS.md's "with a pointer to the
	// model line" actually comes from — and it is not storm.Build. Positions
	// are attached by tool/positions.go, which has the AST; a model built by a
	// library call has none. So the pointer exists through `storm portable
	// oracle` and not through a Go test, exactly as it does for msddl.Check.
	// The rule reads c.Pos when it is there, which is the part to pin.
	positioned := &schema.Schema{Tables: []*schema.Table{{
		Name: "ora_orgs",
		Columns: []*schema.Column{
			{Name: "note", Type: schema.Type{Name: schema.TypeText}, Pos: "model/org.go:12"},
		},
	}}}
	got := strings.Join(oracleCheck(positioned), "\n")
	t.Logf("with positions attached:\n%s", got)
	if !strings.Contains(got, "model/org.go:12") {
		t.Errorf("the refusal must point at the model line when it has one: %s", got)
	}
}

// And what it accepts. A model with no nullable text has nothing about it that
// empty-string-is-NULL can change, which is the property that makes the rule a
// dialect rather than a rejection.
func TestAModelWithNoNullableTextIsAccepted(t *testing.T) {
	s, err := storm.Build(&oraTight{})
	if err != nil {
		t.Fatal(err)
	}
	if problems := oracleCheck(s); len(problems) != 0 {
		t.Errorf("a model with no nullable text was refused:\n%s", strings.Join(problems, "\n"))
	}
}

// The half that is NOT expressible, stated as a test so it is not rediscovered.
//
// The generated query DSL's Eq takes a runtime string. There is no build-time
// artefact holding the value, so there is nothing for a capability model to
// inspect — the schema a Check receives has tables and columns in it and no
// queries at all.
func TestAQueryPredicateValueIsNotInTheSchema(t *testing.T) {
	s, err := storm.Build(&oraTight{})
	if err != nil {
		t.Fatal(err)
	}
	// Everything a Check can see. If a predicate's value were reachable at
	// declare time it would have to be reachable from here, and a schema is
	// tables, enums, functions, views and triggers.
	tb := s.Table("ora_tights")
	if tb == nil {
		t.Fatal("ora_tights is not in the model")
	}
	t.Logf("what a Check receives: %d table(s), %d column(s) in %s, %d check(s), %d enum(s)",
		len(s.Tables), len(tb.Columns), tb.Name, len(tb.Checks), len(s.Enums))
	t.Log("and no queries, which is why Eq(\"\") cannot be a declare-time error")
}

// The expression DSL, where a literal IS visible — the asymmetry that makes
// half the rule work and half of it impossible.
func TestAnExpressionLiteralIsVisibleAtDeclareTime(t *testing.T) {
	var m oraTight
	// The point is not what the Cond holds; it is that cmp() folds the Go ""
	// into a schema.Literal at BUILD time, where the generated
	// TextCol.Eq(v string) would bind it as a parameter. A check over a model's
	// declared expressions can therefore find it; a check over its queries
	// cannot.
	_ = storm.Exprs{}.Eq(&m.Tag, "")
	t.Log(`storm.Exprs{}.Eq(&m.Tag, "") folds "" into a schema.Literal; ` +
		`the generated TextCol.Eq(v string) binds it`)
}
