package msbench

import (
	"testing"

	"github.com/gsoultan/storm/compile/mssql"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/schema"
)

// The DECLARED reads: aggregations, joins and unions. Their SQL is fixed at
// generate time — no token stream numbers it — so every parameter in them is
// this package's to number, and every keyword is this package's to choose.
//
// This is the half where SQL Server is closest to PostgreSQL and furthest from
// MySQL: GROUPING SETS, CUBE, ROLLUP and GROUPING() all exist, and M9 had to
// refuse every one of them.
func TestDeclaredReadsRunOnTheServer(t *testing.T) {
	db := open(t)
	setup(t, db)
	s := msSchema(t)

	table := func(name string) *schema.Table {
		t.Helper()
		for _, tb := range s.Tables {
			if tb.Name == name {
				return tb
			}
		}
		t.Fatalf("no table %s in the built schema", name)
		return nil
	}

	orgs := table("ms_orgs")
	members := table("ms_members")

	for _, agg := range orgs.Aggregates {
		t.Run("aggregate: "+agg.Name, func(t *testing.T) {
			prefix, err := mssql.AggregateSelect(orgs.Name, agg)
			if err != nil {
				t.Fatalf("no lowering: %v", err)
			}
			suffix, err := mssql.AggregateSuffix(agg)
			if err != nil {
				t.Fatalf("no lowering: %v", err)
			}
			run(t, db, lowered{name: agg.Name, sql: prefix + suffix})
		})
	}

	for _, j := range members.Joins {
		t.Run("join: "+j.Name, func(t *testing.T) {
			aggFor := func(c schema.CTE) (string, string) {
				for _, tb := range s.Tables {
					for _, a := range tb.Aggregates {
						if a.Name != c.Aggregate {
							continue
						}
						p, err := mssql.AggregateSelect(tb.Name, a)
						if err != nil {
							t.Fatalf("no lowering for CTE %s: %v", c.Aggregate, err)
						}
						sfx, err := mssql.AggregateSuffix(a)
						if err != nil {
							t.Fatalf("no lowering for CTE %s: %v", c.Aggregate, err)
						}
						return p, sfx
					}
				}
				t.Fatalf("join names a CTE %q that is not declared", c.Aggregate)
				return "", ""
			}
			live := func(tb, alias string) mssql.Live {
				for _, x := range s.Tables {
					if x.Name == tb && x.SoftDelete != "" {
						return mssql.Live(mssql.LiveFor(alias, x.SoftDelete))
					}
				}
				return ""
			}
			prefix, err := mssql.JoinSelect(members.Name, j, aggFor, live)
			if err != nil {
				t.Fatalf("no lowering: %v", err)
			}
			suffix, err := mssql.JoinSuffix(j)
			if err != nil {
				t.Fatalf("no lowering: %v", err)
			}
			where, err := mssql.JoinDeclaredWhere(j, mssql.Live(mssql.LiveFor(members.Name, members.SoftDelete)))
			if err != nil {
				t.Fatalf("no lowering: %v", err)
			}
			sql := prefix
			if where != "" {
				sql += " WHERE " + where
			}
			run(t, db, lowered{name: j.Name, sql: sql + suffix})
		})
	}

	for _, u := range s.Unions {
		t.Run("union: "+u.Name, func(t *testing.T) {
			if err := mssql.UnionOrderRefused(u); err != nil {
				t.Fatalf("no lowering: %v", err)
			}
			live := func(tb string) mssql.Live {
				for _, x := range s.Tables {
					if x.Name == tb && x.SoftDelete != "" {
						return mssql.Live(mssql.LiveFor("", x.SoftDelete))
					}
				}
				return ""
			}
			sql, err := mssql.UnionSelect(u, live)
			if err != nil {
				t.Fatalf("no lowering: %v", err)
			}
			args := make([]any, 0, len(u.Params)+1)
			for range u.Params {
				args = append(args, orgID)
			}
			args = append(args, int64(10))
			run(t, db, lowered{name: u.Name, sql: sql + mssql.UnionSuffix(u), args: args})
		})
	}

	// The refusals, asserted rather than assumed: a construct with no honest
	// lowering must say so by name, and a test that only checks the successes
	// cannot tell "refused" from "silently dropped".
	t.Run("refusals name the construct", func(t *testing.T) {
		for _, op := range []string{"JSONContains", "JSONContainedBy", "HasAllKeys"} {
			if mssql.Supported(op) {
				t.Errorf("%s reports supported; SQL Server has no containment predicate", op)
			}
			if mssql.Refused(op) == "" {
				t.Errorf("%s is unsupported and says nothing about why", op)
			}
		}
		if _, _, ok := mssql.Frag("JSONContains", "[doc]"); ok {
			t.Error("Frag lowered JSONContains, which has no SQL Server form")
		}
	})
}

// A declared parameter used in two branches binds ONCE here, because parameters
// are named. compile/mysql refuses that outright — position is what binds a
// value there, so the second use would be a second binding and a different
// arity than the caller was generated against.
func TestOneNameBindsBothBranches(t *testing.T) {
	sql, err := mssql.Expr(schema.Expr{Kind: schema.ExprParam, Param: 0})
	if err != nil {
		t.Fatal(err)
	}
	if sql != "@p1" {
		t.Fatalf("declared parameter 0 rendered %q, want @p1", sql)
	}
	again, err := mssql.Expr(schema.Expr{Kind: schema.ExprParam, Param: 0})
	if err != nil {
		t.Fatalf("the same parameter used twice was refused: %v", err)
	}
	if again != sql {
		t.Fatalf("one parameter rendered two ways: %q and %q", sql, again)
	}
}

var _ = runtime.MSSQLPlaceholder
