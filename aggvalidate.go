package storm

import (
	"fmt"
	"strings"

	"github.com/gsoultan/storm/schema"
)

// validateAggregate rejects an aggregation PostgreSQL would refuse.
//
// In a grouped query every column reference has to be either one of the
// grouping expressions or inside an aggregate — otherwise there is no single
// value for it in the group. PostgreSQL says so at execution:
//
//	column "orders.placed_at" must appear in the GROUP BY clause
//	or be used in an aggregate function
//
// which is a correct message arriving at the worst time, from a report that may
// only run at month end. The window clauses are where this is easiest to get
// wrong: `row_number() OVER (ORDER BY placed_at)` next to
// `GROUP BY date_trunc('day', placed_at)` looks reasonable and is not.
func validateAggregate(tbl *schema.Table, agg *schema.Aggregate) error {
	for _, p := range agg.Params {
		if p.Type.Name == "" {
			return fmt.Errorf(
				"%s: aggregate %q declares parameter %q and never compares it with "+
					"a column — it would sit in the generated signature demanding an "+
					"argument that reaches no statement",
				tbl.Name, agg.Name, p.Name)
		}
	}
	if len(agg.By) == 0 {
		// No GROUP BY: the whole table is one group and every output must be
		// an aggregate. A bare column here is the same error by another route.
		for _, t := range agg.Terms {
			if col, bad := ungrouped(t.Expr, nil); bad {
				return fmt.Errorf(
					"%s: aggregate %q selects column %s but groups by nothing — "+
						"with no grouping the whole table is one group, so every output "+
						"must be an aggregate",
					tbl.Name, agg.Name, col)
			}
		}
		// The same rule reaches HAVING: with no GROUP BY there is still no
		// single value of a bare column for the one group.
		if agg.Having != nil {
			if col, bad := ungroupedCond(*agg.Having, nil); bad {
				return fmt.Errorf(
					"%s: aggregate %q has a HAVING on column %s but groups by nothing — "+
						"wrap it in an aggregate, or reference a declared output with storm.Out(...)",
					tbl.Name, agg.Name, col)
			}
		}
		return nil
	}

	grouped := make([]schema.Expr, 0, len(agg.By))
	for _, g := range agg.By {
		grouped = append(grouped, g.Expr)
	}
	for _, t := range agg.Terms {
		// Named before the general rule, because the general message sends the
		// reader to "group by it or wrap it in an aggregate" and neither is the
		// fix. The fix is the nested form, which SumOver and its siblings build.
		if col, bad := windowedOverRow(t.Expr, grouped); bad {
			return fmt.Errorf(
				"%s: aggregate %q windows %s(%s) in %q, but the query is grouped — "+
					"with OVER it is a window function, so its argument is read from the "+
					"GROUPED rows and column %s is not there\n"+
					"       to aggregate ACROSS the groups use %sOver(handle, %q, window), "+
					"which builds %s(%s(...)) OVER (...)",
				tbl.Name, agg.Name, t.Expr.Fn, col, t.As, col,
				exportedFn(t.Expr.Fn), t.As, t.Expr.Fn, t.Expr.Fn)
		}
		if col, bad := ungrouped(t.Expr, grouped); bad {
			return fmt.Errorf(
				"%s: aggregate %q reads column %s in %q, but groups by something else — "+
					"a column in a grouped query must be one of the grouping expressions "+
					"or inside an aggregate\n"+
					"       group by it, wrap it in an aggregate, or reference a declared "+
					"output with storm.Out(...)",
				tbl.Name, agg.Name, col, t.As)
		}
	}
	if agg.Having != nil {
		if col, bad := ungroupedCond(*agg.Having, grouped); bad {
			return fmt.Errorf(
				"%s: aggregate %q has a HAVING on column %s, which is neither grouped "+
					"nor aggregated", tbl.Name, agg.Name, col)
		}
	}
	return nil
}

// ungrouped walks an expression and returns the first column reference that is
// neither one of the grouping expressions nor inside an aggregate.
//
// An aggregate's ARGUMENTS and its FILTER are exempt: both are evaluated per
// row within the group. Its OVER clause is not — a window over grouped rows
// sees one row per group.
func ungrouped(e schema.Expr, grouped []schema.Expr) (string, bool) {
	// Checked WHOLE first, before descending. PostgreSQL allows any expression
	// that appears in the GROUP BY, not just bare columns — so
	// date_trunc('day', placed_at) is fine even though placed_at alone is not,
	// and descending into its arguments first would report the opposite.
	for _, g := range grouped {
		if exprEqual(e, g) {
			return "", false
		}
	}
	switch e.Kind {
	case schema.ExprCol:
		return e.Col, true

	case schema.ExprAgg:
		// An aggregate's arguments AND its FILTER are evaluated per row inside
		// the group, so both may read any column — `count(*) FILTER (WHERE
		// total >= 50)` alongside `GROUP BY status` is valid, and asserted
		// against a live server by the example's TestAggregateFilter.
		//
		// UNLESS it has an OVER clause. Then it is a window function call, not
		// an aggregate: its arguments are evaluated over the query's OUTPUT
		// rows, which in a grouped query are groups. `sum(total) OVER (...)`
		// alongside `GROUP BY status` reads a column that no longer exists per
		// output row, and PostgreSQL says so — "column must appear in the GROUP
		// BY clause or be used in an aggregate function". The form that means
		// "across the groups" is sum(sum(total)) OVER (...), which is what
		// SumOver and its siblings build.
		if e.Over != nil {
			for _, a := range e.Args {
				if col, bad := ungrouped(a, grouped); bad {
					return col, true
				}
			}
		}
		// The OVER clause itself is checked the same way: a window over grouped
		// rows sees one row per group, so it may only name grouping
		// expressions or aggregates.
		return ungroupedWindow(e.Over, grouped)

	case schema.ExprGrouping:
		// GROUPING() names grouping columns by definition.
		return "", false

	case schema.ExprWindow:
		for _, a := range e.Args {
			if col, bad := ungrouped(a, grouped); bad {
				return col, true
			}
		}
		return ungroupedWindow(e.Over, grouped)

	default:
		for _, a := range e.Args {
			if col, bad := ungrouped(a, grouped); bad {
				return col, true
			}
		}
	}
	return "", false
}

func ungroupedWindow(w *schema.Window, grouped []schema.Expr) (string, bool) {
	if w == nil {
		return "", false
	}
	for _, p := range w.PartitionBy {
		if col, bad := ungrouped(p, grouped); bad {
			return col, true
		}
	}
	for _, o := range w.OrderBy {
		if col, bad := ungrouped(o.Expr, grouped); bad {
			return col, true
		}
	}
	return "", false
}

func ungroupedCond(c schema.Cond, grouped []schema.Expr) (string, bool) {
	switch c.Kind {
	case schema.CondCmp:
		if col, bad := ungrouped(c.Left, grouped); bad {
			return col, true
		}
		return ungrouped(c.Right, grouped)
	case schema.CondIsNull, schema.CondIsNotNull:
		return ungrouped(c.Left, grouped)
	default:
		for _, a := range c.Args {
			if col, bad := ungroupedCond(a, grouped); bad {
				return col, true
			}
		}
	}
	return "", false
}

// exprEqual is structural equality, which is how PostgreSQL decides whether an
// expression "appears in the GROUP BY". date_trunc('day', placed_at) in the
// SELECT matches date_trunc('day', placed_at) in the GROUP BY; a different unit
// does not.
func exprEqual(a, b schema.Expr) bool {
	if a.Kind != b.Kind || a.Col != b.Col || a.Fn != b.Fn || a.Arith != b.Arith ||
		len(a.Args) != len(b.Args) {
		return false
	}
	if a.Kind == schema.ExprLit && a.Lit != b.Lit {
		return false
	}
	for i := range a.Args {
		if !exprEqual(a.Args[i], b.Args[i]) {
			return false
		}
	}
	return true
}

// windowedOverRow reports an aggregate that carries a window AND reads a column
// straight from the table. It is the one shape whose generic diagnosis points
// the wrong way, so it is diagnosed on its own.
func windowedOverRow(e schema.Expr, grouped []schema.Expr) (string, bool) {
	if e.Kind != schema.ExprAgg || e.Over == nil {
		return "", false
	}
	for _, a := range e.Args {
		if col, bad := ungrouped(a, grouped); bad {
			return col, true
		}
	}
	return "", false
}

// exportedFn is the builder method name for an aggregate: sum → Sum.
func exportedFn(fn string) string {
	if fn == "" {
		return fn
	}
	return strings.ToUpper(fn[:1]) + fn[1:]
}
