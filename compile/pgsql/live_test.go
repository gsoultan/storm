package pgsql_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/pgsql"
	"github.com/gsoultan/storm/schema"
)

// Where the soft-delete predicate goes is not a detail — in an outer join it is
// the whole behaviour of the join, and in a union it is the difference between
// filtering a table and naming a column the other branch does not have.

func live(table, alias string) pgsql.Live {
	if table != "members" { // only members soft-deletes
		return ""
	}
	return pgsql.LiveFor(alias, "deleted_at")
}

func memberJoin(kind schema.JoinKind) *schema.Join {
	return &schema.Join{
		Name: "OrgMembers",
		Tables: []schema.JoinTable{{
			Kind: kind, Table: "members", Alias: "members",
			On: schema.Cond{Kind: schema.CondCmp, Op: schema.OpEq,
				Left:  schema.Expr{Kind: schema.ExprCol, Tbl: "members", Col: "org_id"},
				Right: schema.Expr{Kind: schema.ExprCol, Tbl: "orgs", Col: "id"}},
		}},
		Select: []schema.JoinCol{
			{Expr: schema.Expr{Kind: schema.ExprCol, Tbl: "orgs", Col: "name"}, As: "Name"},
		},
	}
}

// The one that would be silently wrong. A joined table's predicate belongs in
// ON, not WHERE: in WHERE, a parent whose only child is deleted is DROPPED and
// the LEFT JOIN quietly becomes an INNER JOIN. In ON, the parent survives with
// a NULL-extended child — which is what it gets when it has no child at all.
func TestJoinedTablePredicateGoesInTheOnClause(t *testing.T) {
	got := pgsql.JoinSelect("orgs", memberJoin(schema.JoinLeft), nil, live)

	on := got[strings.Index(got, " ON "):]
	if !strings.Contains(on, `"members"."deleted_at" IS NULL`) {
		t.Errorf("the child predicate is not in the ON clause:\n%s", got)
	}
	if strings.Contains(got, " WHERE ") {
		t.Errorf("a joined table's predicate reached the WHERE — an outer join "+
			"filtered there is an inner join:\n%s", got)
	}
	// Qualified. Unqualified would be ambiguous the moment both tables have the
	// column, and wrong when only one does.
	if strings.Contains(got, ` "deleted_at" IS NULL`) {
		t.Errorf("the predicate is not qualified by alias:\n%s", got)
	}
}

// The driving table has no ON clause of its own, and excluding its marked rows
// is exactly what filtering the driving table should do.
func TestDrivingTablePredicateGoesInTheWhere(t *testing.T) {
	j := memberJoin(schema.JoinInner)
	if w := pgsql.JoinDeclaredWhere(j, live("members", "members")); w != `"members"."deleted_at" IS NULL` {
		t.Errorf("driving-table predicate is %q", w)
	}
	// And it composes with a declared filter rather than replacing it.
	j.Where = &schema.Cond{Kind: schema.CondIsNull,
		Left: schema.Expr{Kind: schema.ExprCol, Tbl: "orgs", Col: "closed_at"}}
	w := pgsql.JoinDeclaredWhere(j, live("members", "members"))
	if !strings.Contains(w, "closed_at") || !strings.Contains(w, "deleted_at") {
		t.Errorf("the declared filter and the predicate did not compose: %q", w)
	}
}

// A union's branches read different tables and only some of them soft-delete.
func TestUnionFiltersOnlyTheBranchesThatNeedIt(t *testing.T) {
	u := &schema.Union{
		Name: "Everything",
		Cols: []schema.UnionCol{{As: "Name"}},
		Branches: []schema.UnionBranch{
			{Table: "orgs", Exprs: []schema.Expr{{Kind: schema.ExprCol, Col: "name"}}},
			{Table: "members", Exprs: []schema.Expr{{Kind: schema.ExprCol, Col: "email"}}},
		},
	}
	got := pgsql.UnionSelect(u, func(tb string) pgsql.Live { return live(tb, "") })
	orgs := got[:strings.Index(got, "UNION")]
	members := got[strings.Index(got, "UNION"):]
	if strings.Contains(orgs, "deleted_at") {
		t.Errorf("a branch that does not soft-delete was filtered:\n%s", orgs)
	}
	if !strings.Contains(members, "deleted_at") {
		t.Errorf("the soft-delete branch was not filtered:\n%s", members)
	}
}

// A per-parent load must not spend its N slots on deleted rows: a parent whose
// most recent N children are all deleted would get an empty page.
func TestTopNExcludesDeletedBeforeTheLimit(t *testing.T) {
	for name, got := range map[string]string{
		"lateral": pgsql.TopNLateral("members", []string{"id"}, "org_id", "uuid", live("members", "")),
		"window":  pgsql.TopNWindow("members", []string{"id"}, "org_id", live("members", "")),
	} {
		if !strings.Contains(got, "deleted_at") {
			t.Errorf("%s: no predicate:\n%s", name, got)
		}
		if i, j := strings.Index(got, "deleted_at"), strings.Index(got, "LIMIT"); j >= 0 && i > j {
			t.Errorf("%s: the predicate is applied after the limit:\n%s", name, got)
		}
	}
}

// "This parent has a related row" must not be satisfied by a row the child's
// own package would refuse to return.
func TestExistsSemiJoinExcludesDeletedChildren(t *testing.T) {
	got := pgsql.ExistsFrag("members", "org_id", "orgs", "id", live("members", ""))
	if !strings.Contains(got, `"_storm_e"."deleted_at" IS NULL`) {
		t.Errorf("the semi-join does not exclude deleted children, or does not qualify:\n%s", got)
	}
}

// A recursive read touches the table twice. Guarding only the anchor lets a
// deleted row back in on the second iteration, carrying its whole subtree.
func TestRecursiveGuardsBothHalves(t *testing.T) {
	got := pgsql.Recursive("members", []string{"id", "org_id"}, "id", "parent_id",
		pgsql.Descend, live("members", ""))
	if n := strings.Count(got, "deleted_at"); n != 2 {
		t.Errorf("the predicate appears %d time(s); the anchor and the recursive term both need it:\n%s", n, got)
	}
	if !strings.Contains(got, `"_storm_rc"."deleted_at" IS NULL`) {
		t.Errorf("the recursive term's predicate is not qualified to the child:\n%s", got)
	}
}

// A table that does not soft-delete must come out exactly as before.
func TestNoPredicateWhenTheTableDoesNotSoftDelete(t *testing.T) {
	none := func(string, string) pgsql.Live { return "" }
	got := pgsql.JoinSelect("orgs", memberJoin(schema.JoinLeft), nil, none)
	if strings.Contains(got, "deleted_at") || strings.Contains(got, " AND ") {
		t.Errorf("a hard-delete join gained a predicate:\n%s", got)
	}
	if w := pgsql.JoinDeclaredWhere(memberJoin(schema.JoinInner), ""); w != "" {
		t.Errorf("declared WHERE is %q, want empty", w)
	}
}
