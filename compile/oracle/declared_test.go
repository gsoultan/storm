package oracle_test

// The declared reads — joins, unions and aggregations — and the write path.
//
// internal/oraclespike runs these against a server, but that is a separate
// module and a server is not always there. What a live gate cannot say is
// WHICH decision produced the bytes: `FROM t x` and `FROM t AS x` are both
// accepted by some server, and only one of them is this dialect's. So the
// keyword is asserted here and the acceptance there.

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/oracle"
	"github.com/gsoultan/storm/schema"
)

func col(name string) schema.Expr { return schema.Expr{Kind: schema.ExprCol, Col: name} }

func agg(sets *schema.GroupingSets, order []schema.AggOrder) *schema.Aggregate {
	return &schema.Aggregate{
		Name: "ByRegion",
		By: []schema.GroupTerm{
			{Expr: col("region"), As: "Region"},
			{Expr: col("tier"), As: "Tier"},
		},
		Terms: []schema.AggregateTerm{
			{Expr: schema.Expr{Kind: schema.ExprAgg, Fn: "sum", Args: []schema.Expr{col("amt")}},
				As: "Total"},
		},
		Sets:    sets,
		OrderBy: order,
	}
}

// All three grouping-set forms exist here, in the FUNCTION spelling — measured
// on a live server before this package was written. compile/mysql refuses two
// of them and spells the third as a suffix.
func TestGroupingSetsAreFunctions(t *testing.T) {
	for _, c := range []struct {
		kind schema.GroupingSetsKind
		want string
	}{
		{schema.SetsRollup, `GROUP BY ROLLUP("region", "tier")`},
		{schema.SetsCube, `GROUP BY CUBE("region", "tier")`},
	} {
		got, err := oracle.GroupBy(agg(&schema.GroupingSets{Kind: c.kind}, nil))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(got) != c.want {
			t.Errorf("got  %s\nwant %s", strings.TrimSpace(got), c.want)
		}
		if strings.Contains(got, "WITH ROLLUP") {
			t.Errorf("MySQL's suffix form reached Oracle SQL: %s", got)
		}
	}
	// And the plain grouping.
	got, err := oracle.GroupBy(agg(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != `GROUP BY "region", "tier"` {
		t.Errorf("got %s", strings.TrimSpace(got))
	}
}

// A rollup's subtotal rows belong ABOVE the detail they summarise, and a
// subtotal has NULL in the column it totals. Oracle sorts NULLs LAST ascending
// — PostgreSQL's arrangement, the opposite of SQL Server's — so the placement
// is needed on the ASCENDING side and is SPELLED rather than computed.
func TestSubtotalsSortAboveTheirDetailOnTheAscendingSide(t *testing.T) {
	asc, err := oracle.AggregateSuffix(agg(&schema.GroupingSets{Kind: schema.SetsRollup},
		[]schema.AggOrder{{As: "Region"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asc, `"region" NULLS FIRST`) {
		t.Errorf("ascending needs the placement here, where SQL Server needs a CASE: %s", asc)
	}
	if strings.Contains(asc, "CASE WHEN") {
		t.Errorf("Oracle can spell the placement; the CASE is SQL Server's workaround: %s", asc)
	}
	// Descending already puts NULLs first, so nothing is added.
	desc, err := oracle.AggregateSuffix(agg(&schema.GroupingSets{Kind: schema.SetsRollup},
		[]schema.AggOrder{{As: "Region", Desc: true}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(desc, "NULLS") {
		t.Errorf("descending sorts NULLs first already: %s", desc)
	}
}

func TestAggregateSelectAliasesEveryOutput(t *testing.T) {
	got, err := oracle.AggregateSelect("orders", agg(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	// The alias goes through columnCase, which is shared rather than restated:
	// an alias is part of the generated Go type's field name, and two
	// spellings of it would be two structs for one declaration.
	for _, want := range []string{`AS "region"`, `AS "tier"`, `AS "total"`, `FROM "orders"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestHavingFollowsTheGrouping(t *testing.T) {
	a := agg(nil, nil)
	cond := schema.Cond{Kind: schema.CondCmp, Op: schema.OpGt,
		Left:  schema.Expr{Kind: schema.ExprAgg, Fn: "sum", Args: []schema.Expr{col("amt")}},
		Right: schema.Expr{Kind: schema.ExprLit, Lit: schema.Literal{Kind: schema.TypeInt8, I: 10}}}
	a.Having = &cond
	got, err := oracle.AggregateSuffix(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(got, "HAVING") < strings.Index(got, "GROUP BY") {
		t.Errorf("HAVING must follow GROUP BY: %s", got)
	}
}

// A TABLE alias is juxtaposition here — `FROM t AS x` is ORA-00933 — where a
// COLUMN alias still takes AS. Both appear in one statement, which is why this
// is easy to get wrong and still compile.
func TestATableAliasTakesNoAS(t *testing.T) {
	j := &schema.Join{
		Name:   "OrderWithCustomer",
		Select: []schema.JoinCol{{Expr: col("id"), As: "ID"}},
		Tables: []schema.JoinTable{{
			Table: "customers", Alias: "c", Kind: schema.JoinInner,
			On: schema.Cond{Kind: schema.CondCmp, Op: schema.OpEq,
				Left:  schema.Expr{Kind: schema.ExprCol, Tbl: "orders", Col: "customer_id"},
				Right: schema.Expr{Kind: schema.ExprCol, Tbl: "c", Col: "id"}},
		}},
	}
	got, err := oracle.JoinSelect("orders", j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, `"customers" AS "c"`) {
		t.Errorf("a table alias by AS is ORA-00933: %s", got)
	}
	if !strings.Contains(got, `"customers" "c"`) {
		t.Errorf("want juxtaposition: %s", got)
	}
	// The COLUMN alias still takes AS, in the same statement.
	if !strings.Contains(got, `AS "id"`) {
		t.Errorf("a column alias takes AS: %s", got)
	}
}

// A joined table's soft-delete predicate goes in its ON clause, never the
// WHERE: in the WHERE, a parent whose only child is deleted is DROPPED and the
// outer join silently becomes an inner one.
func TestJoinedSoftDeleteGoesInTheOnClause(t *testing.T) {
	j := &schema.Join{
		Name:   "J",
		Select: []schema.JoinCol{{Expr: col("id"), As: "ID"}},
		Tables: []schema.JoinTable{{
			Table: "children", Alias: "c", Kind: schema.JoinLeft,
			On: schema.Cond{Kind: schema.CondCmp, Op: schema.OpEq,
				Left:  schema.Expr{Kind: schema.ExprCol, Tbl: "parents", Col: "id"},
				Right: schema.Expr{Kind: schema.ExprCol, Tbl: "c", Col: "parent_id"}},
		}},
	}
	got, err := oracle.JoinSelect("parents", j,
		func(schema.CTE) (string, string) { return "", "" },
		func(table, alias string) oracle.Live {
			return oracle.Live(oracle.LiveFor(alias, "deleted_at"))
		})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `LEFT JOIN`) {
		t.Errorf("the join kind was lost: %s", got)
	}
	on := got[strings.Index(got, " ON "):]
	if !strings.Contains(on, `"c"."deleted_at" IS NULL`) {
		t.Errorf("the child's predicate must be in ON, not WHERE: %s", got)
	}
}

// A declared parameter used in TWO branches binds one value passed once — `:1`
// is a NAME. compile/mysql has to refuse that outright, because position is
// what binds there.
func TestAUnionReusesADeclaredParameter(t *testing.T) {
	u := &schema.Union{
		Name:   "Feed",
		Cols:   []schema.UnionCol{{As: "Title"}},
		Params: []schema.Param{{Name: "who"}},
		Branches: []schema.UnionBranch{
			{Table: "posts", Exprs: []schema.Expr{col("title")},
				Where: condEqParam("posts", "author")},
			{Table: "events", Exprs: []schema.Expr{col("label")},
				Where: condEqParam("events", "author")},
		},
	}
	got, err := oracle.UnionSelect(u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, ":1") != 2 {
		t.Errorf("the same parameter must render as the same name in both branches: %s", got)
	}
	if !strings.Contains(got, "UNION ALL") {
		t.Errorf("duplicates are kept unless asked: %s", got)
	}
}

func condEqParam(tbl, col string) *schema.Cond {
	c := schema.Cond{Kind: schema.CondCmp, Op: schema.OpEq,
		Left:  schema.Expr{Kind: schema.ExprCol, Tbl: tbl, Col: col},
		Right: schema.Expr{Kind: schema.ExprParam, Param: 0}}
	return &c
}

// The row cap is FETCH FIRST, and a union with no declared ordering gets NONE
// invented — FETCH FIRST is not a clause of ORDER BY here, which is the one
// place this is simpler than SQL Server.
func TestUnionSuffixInventsNoOrdering(t *testing.T) {
	u := &schema.Union{Name: "Feed", Cols: []schema.UnionCol{{As: "Title"}}}
	got := oracle.UnionSuffix(u)
	if !strings.Contains(got, "FETCH FIRST") {
		t.Errorf("want FETCH FIRST: %s", got)
	}
	if strings.Contains(got, "ORDER BY") {
		t.Errorf("nothing has to be invented to satisfy the grammar here: %s", got)
	}
	if strings.Contains(got, "OFFSET 0") {
		t.Errorf("OFFSET is optional here, unlike SQL Server: %s", got)
	}
}

// Oracle has NULLS FIRST/LAST wherever an ORDER BY appears, so unlike SQL
// Server there is no placement to refuse.
func TestUnionOrderingSpellsItsNullPlacement(t *testing.T) {
	yes := true
	u := &schema.Union{Name: "Feed", Cols: []schema.UnionCol{{As: "Title"}},
		OrderBy: []schema.UnionOrder{{Col: "Title", NullsFirst: &yes}}}
	if err := oracle.UnionOrderRefused(u); err != nil {
		t.Errorf("Oracle can spell this: %v", err)
	}
	if got := oracle.UnionSuffix(u); !strings.Contains(got, "NULLS FIRST") {
		t.Errorf("the placement must be emitted: %s", got)
	}
}

// Recursion carries NO path column: the CYCLE clause is the server's own guard.
func TestRecursionUsesTheServersCycleGuard(t *testing.T) {
	got := oracle.Recursive("tree", []string{"id", "parent_id"}, "id", "parent_id",
		"NUMBER(19)", oracle.Descend, "")
	for _, want := range []string{"CYCLE", `SET "_storm_cycle" TO 'Y' DEFAULT 'N'`, "UNION ALL"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// None of the machinery the other three need.
	for _, wrong := range []string{"_storm_path", "CHARINDEX", "CONVERT", "MAXRECURSION", "OPENJSON"} {
		if strings.Contains(got, wrong) {
			t.Errorf("%s reached Oracle's recursion, which needs no path: %s", wrong, got)
		}
	}
	if !strings.Contains(got, "JSON_TABLE") {
		t.Errorf("the roots arrive as one bound document: %s", got)
	}
}

func TestRecursionGuardsBothHalvesAgainstSoftDelete(t *testing.T) {
	live := oracle.Live(oracle.LiveFor("", "deleted_at"))
	got := oracle.Recursive("tree", []string{"id"}, "id", "parent_id", "NUMBER(19)",
		oracle.Ascend, live)
	// Guarding only the anchor lets a deleted row back in on the second
	// iteration, carrying its whole subtree with it.
	if strings.Count(got, "deleted_at") < 2 {
		t.Errorf("both halves must be guarded: %s", got)
	}
	if !strings.Contains(got, `"_storm_rc"."deleted_at"`) {
		t.Errorf("the recursive half's predicate must name the CHILD: %s", got)
	}
}

// The cap is FETCH FIRST after the ordering, not TOP before the column list.
func TestTopNLateralCapsWithFetchFirst(t *testing.T) {
	got := oracle.TopNLateral("posts", []string{"id", "title"}, "author_id", "NUMBER(19)", "")
	if strings.Contains(got, "TOP (") {
		t.Errorf("TOP is SQL Server's: %s", got)
	}
	if !strings.Contains(got, "FETCH FIRST :2 ROWS ONLY") {
		t.Errorf("want FETCH FIRST with the cap's ordinal: %s", got)
	}
	if !strings.Contains(got, "CROSS APPLY") {
		t.Errorf("CROSS APPLY is Oracle's LATERAL: %s", got)
	}
	if strings.Contains(got, `) AS "`) {
		t.Errorf("a derived table's alias is juxtaposition: %s", got)
	}
}

func TestTopNWindowNumbersRowsPerParent(t *testing.T) {
	got := oracle.TopNWindow("posts", []string{"id"}, "author_id", "NUMBER(19)", "")
	for _, want := range []string{"row_number() OVER (PARTITION BY", "JSON_TABLE", ":2"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "OPENJSON") {
		t.Errorf("OPENJSON is SQL Server's: %s", got)
	}
}

// The write path, and the refusal that defines it.
func TestTheWritePathRefusesAReturningList(t *testing.T) {
	if _, err := oracle.InsertStmt("t", []string{"a"}, []string{"a"}); err == nil {
		t.Fatal("a returning list must be refused, not dropped")
	} else if !strings.Contains(err.Error(), "OUTPUT parameters") {
		t.Errorf("the refusal must say WHY — the clause exists and the port cannot "+
			"carry its binds: %v", err)
	}
	if got := oracle.ReturningClause([]string{"id"}); got != "" {
		t.Errorf("no returning clause is ever emitted, got %q", got)
	}
}

func TestSoftDeleteStampsTheServersClock(t *testing.T) {
	if got := oracle.SoftDeleteSet("users", "deleted_at"); !strings.Contains(got, "SYSTIMESTAMP") {
		t.Errorf("want the server's own clock, not the session's: %s", got)
	}
	if got := oracle.RestoreSet("users", "deleted_at"); !strings.Contains(got, "= NULL") {
		t.Errorf("got %s", got)
	}
	if got := oracle.SoftDeleteWhere("deleted_at"); got != `"deleted_at" IS NULL` {
		t.Errorf("got %s", got)
	}
}

// A semi-join's alias is quoted for the same reason every internal alias is:
// an unquoted Oracle identifier may not begin with an underscore.
func TestSemiJoinAliasesAreQuoted(t *testing.T) {
	got := oracle.ExistsFrag("comments", "post_id", "posts", "id", "")
	if !strings.Contains(got, `"_storm_x"`) {
		t.Errorf("the alias must be quoted — ORA-00911 otherwise: %s", got)
	}
	if strings.Contains(got, ` AS "_storm_x"`) {
		t.Errorf("a table alias takes no AS: %s", got)
	}
	if !strings.HasPrefix(oracle.NotExistsFrag("comments", "post_id", "posts", "id", ""), "NOT ") {
		t.Error("the negation must be spelled")
	}
	// The child's own soft-delete predicate travels with it, re-qualified:
	// "this parent has a related row" must not be satisfied by a row the
	// child's package would refuse to return.
	live := oracle.Live(oracle.LiveFor("", "deleted_at"))
	withLive := oracle.ExistsFrag("comments", "post_id", "posts", "id", live)
	if !strings.Contains(withLive, `"_storm_x"."deleted_at" IS NULL`) {
		t.Errorf("the child's predicate must be re-qualified to the alias: %s", withLive)
	}
	// And the Open forms are the same text without the closing paren, so the
	// two cannot drift.
	if oracle.ExistsOpen("c", "f", "p", "k", "")+")" != oracle.ExistsFrag("c", "f", "p", "k", "") {
		t.Error("ExistsOpen and ExistsFrag disagree")
	}
}
