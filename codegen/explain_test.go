package codegen_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/codegen"
	"github.com/gsoultan/storm/schema"
)

// `storm explain` is the only gate that asserts every statement storm emits is
// one PostgreSQL will accept. It is worth exactly what it enumerates, and until
// 2026-09-02 it enumerated base reads and relation loads — which is the part
// that could hardly be wrong. The declared aggregations and joins, which carry
// GROUP BY, HAVING, FILTER, grouping sets, window frames and CTEs, were not
// looked at at all.
//
// These fixtures are shaped for that: each one puts a construct into the fixed
// text of a statement, where a lowering bug becomes SQL nobody plans until a
// caller runs it.

type xCustomer struct {
	storm.Model
	Email string
}

type xOrder struct {
	storm.Model
	Customer xCustomer
	Status   string
	Total    storm.Decimal
}

func (o *xOrder) Projections(p *storm.Projections) {
	p.Named("Brief", &o.Status, &o.Total)
}

func (o *xOrder) Aggregates(a *storm.Aggregates) {
	// Everything that lands in the fixed text: a FILTER, a HAVING, a grouping
	// expression and a window function.
	b := a.Named("ByStatus")
	b.By(&o.Status)
	orders := b.Count("Orders")
	b.Count("Big").Filter(a.Gte(&o.Total, a.Lit(50)))
	b.Having(a.Gt(orders, 0))

	d := a.Named("Daily")
	day := d.ByExpr("Day", a.DateTrunc("day", &o.CreatedAt))
	rev := d.Sum(&o.Total, "Revenue")
	d.RowNumber("Rank", a.Over().OrderByDesc(rev))
	d.Lag(rev, "Prev", a.Over().OrderByAsc(day))

	// A grouping set, whose subtotal NULLs need GROUPING() to read.
	f := a.Named("Facets")
	f.By(&o.Status)
	f.Rollup()
	f.Count("N")
	f.GroupingOf("IsSubtotal", &o.Status)

	// The aggregate a CTE will select from.
	c := a.Named("ByCustomer")
	c.By(&o.Customer)
	c.Sum(&o.Total, "Spend")
}

func (o *xOrder) Joins(j *storm.Joins) {
	var c xCustomer
	j.Named("WithCustomer").
		Inner(&c, &o.Customer).
		Take(&o.ID, "OrderID").
		Take(&c.Email, "Email").
		OrderDesc(&o.CreatedAt)

	// A CTE: the join's prefix has to carry a whole other statement.
	j.Named("VsLifetime").
		With("spend", &xOrder{}, "ByCustomer").
		Inner(&c, &o.Customer).
		LeftWith("spend", j.OnCols("spend", "customer_id", &c.ID)).
		Take(&o.ID, "OrderID").
		TakeFrom("spend", "Spend", "Lifetime")
}

func explainByLabel(t *testing.T) map[string]string {
	t.Helper()
	s, err := storm.Build(&xOrder{}, &xCustomer{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tb := range s.Tables {
		names = append(names, tb.Name)
	}
	qs, err := codegen.ExplainQueries(s, names)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, q := range qs {
		if q.SQL == "" {
			t.Errorf("%s has no SQL", q.Label)
		}
		out[q.Label] = q.SQL
	}
	return out
}

func TestExplainCoversDeclaredAggregates(t *testing.T) {
	got := explainByLabel(t)
	for _, tc := range []struct {
		label string
		want  []string
	}{
		// A filtered aggregate and a HAVING both live in the fixed text, so a
		// mislowering of either is a statement no test would otherwise plan.
		{"x_orders → ByStatus (aggregate)", []string{"FILTER (WHERE", "GROUP BY", "HAVING"}},
		// A window function beside an aggregate is the construct PostgreSQL is
		// most particular about: OVER cannot be nested inside the aggregate.
		{"x_orders → Daily (aggregate)", []string{"date_trunc", "OVER (", "row_number()", "lag("}},
		// GROUPING() only means anything with a grouping set, and the two are
		// lowered separately.
		{"x_orders → Facets (aggregate)", []string{"ROLLUP", "GROUPING("}},
	} {
		sql, ok := got[tc.label]
		if !ok {
			t.Errorf("%s was not enumerated", tc.label)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(sql, w) {
				t.Errorf("%s is missing %q:\n%s", tc.label, w, sql)
			}
		}
	}
}

func TestExplainCoversDeclaredJoins(t *testing.T) {
	got := explainByLabel(t)

	sql, ok := got["x_orders → WithCustomer (join)"]
	if !ok {
		t.Fatal("the join was not enumerated")
	}
	// Plain JOIN, not INNER JOIN — the keyword is optional and storm omits it.
	if !strings.Contains(sql, `JOIN "x_customers" ON `) || !strings.Contains(sql, "ORDER BY") {
		t.Errorf("join SQL is not a join:\n%s", sql)
	}

	// The CTE case is the one worth having: the join's prefix carries a whole
	// second statement, and it is assembled by a resolver that can fail.
	cte, ok := got["x_orders → VsLifetime (join)"]
	if !ok {
		t.Fatal("the CTE join was not enumerated")
	}
	for _, w := range []string{"WITH ", `"spend" AS (`, "LEFT JOIN", "GROUP BY"} {
		if !strings.Contains(cte, w) {
			t.Errorf("CTE join is missing %q:\n%s", w, cte)
		}
	}
}

func TestExplainCoversProjections(t *testing.T) {
	got := explainByLabel(t)
	sql, ok := got["x_orders (projection Brief)"]
	if !ok {
		t.Fatal("the projection was not enumerated")
	}
	// The point of a projection is the columns it does NOT read; explaining the
	// base read instead would plan a different statement and prove nothing.
	if strings.Contains(sql, `"created_at"`) {
		t.Errorf("the projection was enumerated as the base read:\n%s", sql)
	}
	if !strings.Contains(sql, `"status"`) || !strings.Contains(sql, `"total"`) {
		t.Errorf("projection is missing its declared columns:\n%s", sql)
	}
}

// Every statement must end up syntactically whole. A join with a declared WHERE
// is assembled from three pieces the runtime normally separates, and dropping
// the WHERE keyword between them is the mistake this shape invites.
func TestExplainStatementsAreWellFormed(t *testing.T) {
	for label, sql := range explainByLabel(t) {
		if !strings.HasPrefix(sql, "SELECT ") && !strings.HasPrefix(sql, "WITH ") {
			t.Errorf("%s does not start a statement:\n%s", label, sql)
		}
		if strings.Contains(sql, "  ") {
			t.Errorf("%s has a doubled space, which is how a dropped keyword looks:\n%s", label, sql)
		}
	}
}

// A CTE names a table and an aggregation by string, because that is what the
// schema IR carries. Both references can miss — a model that renamed an
// aggregate, a context built without the table the CTE reads from — and the
// enumeration has to say which one rather than emitting SQL with an empty WITH
// body, which PostgreSQL would reject at a place bearing no relation to the
// mistake.
func TestExplainReportsBrokenCTEReferences(t *testing.T) {
	for _, tc := range []struct {
		name       string
		break_     func(c *schema.CTE)
		wantSubstr string
	}{
		{"missing table", func(c *schema.CTE) { c.Table = "no_such_table" }, "no_such_table"},
		{"missing aggregate", func(c *schema.CTE) { c.Aggregate = "NoSuchAgg" }, "NoSuchAgg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := storm.Build(&xOrder{}, &xCustomer{})
			if err != nil {
				t.Fatal(err)
			}
			tb := s.Table("x_orders")
			var found bool
			for i := range tb.Joins {
				if len(tb.Joins[i].CTEs) > 0 {
					tc.break_(&tb.Joins[i].CTEs[0])
					found = true
				}
			}
			if !found {
				t.Fatal("no CTE to break; the fixture stopped testing what it is for")
			}

			_, err = codegen.ExplainQueries(s, []string{"x_orders", "x_customers"})
			if err == nil {
				t.Fatal("a CTE naming something that does not exist was enumerated anyway")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error does not name %q: %v", tc.wantSubstr, err)
			}
		})
	}
}
