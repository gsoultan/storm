package pgsql_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
)

func qcol(tbl, name string) schema.Expr {
	return schema.Expr{Kind: schema.ExprCol, Tbl: tbl, Col: name}
}

func joinFixture(kind schema.JoinKind) *schema.Join {
	return &schema.Join{
		Name: "WithCustomer",
		Tables: []schema.JoinTable{{
			Kind: kind, Table: "customers", Alias: "customers",
			On: schema.Cond{Kind: schema.CondCmp, Op: schema.OpEq,
				Left: qcol("orders", "customer_id"), Right: qcol("customers", "id")},
		}},
		Select: []schema.JoinCol{
			{Expr: qcol("orders", "id"), As: "OrderID"},
			{Expr: qcol("customers", "email"), As: "Email"},
		},
		OrderBy: []schema.JoinOrder{{Expr: qcol("orders", "placed_at"), Desc: true}},
	}
}

func noCTE(schema.CTE) (string, string) { return "", "" }

// Every column is qualified. "id" in a two-table query is ambiguous and
// PostgreSQL says so.
func TestJoinQualifiesEveryColumn(t *testing.T) {
	got := pgsql.JoinSelect("orders", joinFixture(schema.JoinInner), noCTE)
	want := `SELECT "orders"."id" AS "order_id", "customers"."email" AS "email" ` +
		`FROM "orders" JOIN "customers" ON "orders"."customer_id" = "customers"."id"`
	if got != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}
}

func TestLeftJoinKeyword(t *testing.T) {
	got := pgsql.JoinSelect("orders", joinFixture(schema.JoinLeft), noCTE)
	if !strings.Contains(got, ` LEFT JOIN "customers" `) {
		t.Errorf("got %s", got)
	}
}

// A join has no natural order, and an unordered multi-table result shuffles
// between requests.
func TestJoinOrderBy(t *testing.T) {
	want := ` ORDER BY "orders"."placed_at" DESC`
	if got := pgsql.JoinSuffix(joinFixture(schema.JoinInner)); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A CTE is emitted as a WITH before the SELECT, and referenced by its alias.
func TestJoinWithCTE(t *testing.T) {
	j := joinFixture(schema.JoinInner)
	j.CTEs = []schema.CTE{{Alias: "spend", Table: "orders", Aggregate: "ByCustomer"}}
	j.Tables = append(j.Tables, schema.JoinTable{
		Kind: schema.JoinLeft, Alias: "spend",
		On: schema.Cond{Kind: schema.CondCmp, Op: schema.OpEq,
			Left: qcol("spend", "customer_id"), Right: qcol("customers", "id")},
	})
	got := pgsql.JoinSelect("orders", j, func(c schema.CTE) (string, string) {
		return `SELECT "customer_id", count(*) AS "n" FROM "orders"`, ` GROUP BY "customer_id"`
	})
	for _, want := range []string{
		`WITH "spend" AS (SELECT "customer_id", count(*) AS "n" FROM "orders" GROUP BY "customer_id")`,
		`LEFT JOIN "spend" ON "spend"."customer_id" = "customers"."id"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from:\n%s", want, got)
		}
	}
	// The WITH must precede the SELECT.
	if strings.Index(got, "WITH ") > strings.Index(got, "SELECT ") {
		t.Errorf("the WITH clause is not first: %s", got)
	}
}

func TestJoinDeclaredWhere(t *testing.T) {
	j := joinFixture(schema.JoinInner)
	if got := pgsql.JoinDeclaredWhere(j); got != "" {
		t.Errorf("a join with no declared Where rendered %q", got)
	}
	j.Where = &schema.Cond{Kind: schema.CondCmp, Op: schema.OpNe,
		Left:  qcol("orders", "status"),
		Right: schema.Expr{Kind: schema.ExprLit, Lit: schema.Literal{Kind: schema.TypeText, S: "cancelled"}}}
	if got := pgsql.JoinDeclaredWhere(j); got != `"orders"."status" <> 'cancelled'` {
		t.Errorf("got %s", got)
	}
}

// A single-table read must NOT gain qualification — the generated SQL for
// every existing query would change for no reason.
func TestUnqualifiedColumnStaysUnqualified(t *testing.T) {
	if got := pgsql.Expr(schema.Expr{Kind: schema.ExprCol, Col: "status"}); got != `"status"` {
		t.Errorf("got %s, want \"status\"", got)
	}
}
