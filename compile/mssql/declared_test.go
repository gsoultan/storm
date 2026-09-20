package mssql_test

// The declared reads — joins, unions and aggregations — which nothing else in
// the root module reaches.
//
// internal/mssqlspike runs every one of these against a server, but that is a
// separate module and a server is not always there. What a live gate cannot say
// is WHICH decision produced the bytes: `GROUP BY ROLLUP(a, b)` and
// `GROUP BY a, b WITH ROLLUP` are both accepted by their own servers, and only
// one of them is this dialect's. So the keyword is asserted here and the
// acceptance there.

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/mssql"
	"github.com/gsoultan/storm/schema"
)

func agg(sets *schema.GroupingSets, order []schema.AggOrder) *schema.Aggregate {
	return &schema.Aggregate{
		Name: "ByRegion",
		By: []schema.GroupTerm{
			{Expr: schema.Expr{Kind: schema.ExprCol, Col: "region"}, As: "Region"},
			{Expr: schema.Expr{Kind: schema.ExprCol, Col: "tier"}, As: "Tier"},
		},
		Terms: []schema.AggregateTerm{
			{Expr: schema.Expr{Kind: schema.ExprAgg, Fn: "sum",
				Args: []schema.Expr{{Kind: schema.ExprCol, Col: "amt"}}}, As: "Total"},
		},
		Sets:    sets,
		OrderBy: order,
	}
}

// All three grouping-set forms exist here, in the FUNCTION spelling.
// compile/mysql refuses two of them outright and spells the third as a suffix.
func TestEveryGroupingSetFormIsEmitted(t *testing.T) {
	for _, c := range []struct {
		kind schema.GroupingSetsKind
		want string
	}{
		{schema.SetsRollup, "GROUP BY ROLLUP([region], [tier])"},
		{schema.SetsCube, "GROUP BY CUBE([region], [tier])"},
	} {
		got, err := mssql.GroupBy(agg(&schema.GroupingSets{Kind: c.kind}, nil))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(got) != c.want {
			t.Errorf("got  %s\nwant %s", strings.TrimSpace(got), c.want)
		}
		if strings.Contains(got, "WITH ROLLUP") {
			t.Errorf("MySQL's suffix form reached SQL Server SQL: %s", got)
		}
	}

	explicit, err := mssql.GroupBy(agg(&schema.GroupingSets{
		Kind: schema.SetsExplicit, Sets: [][]int{{0}, {1}, nil}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := "GROUP BY GROUPING SETS (([region]), ([tier]), ())"
	if strings.TrimSpace(explicit) != want {
		t.Errorf("got  %s\nwant %s", strings.TrimSpace(explicit), want)
	}

	// No sets at all: the plain list, no wrapper.
	plain, err := mssql.GroupBy(agg(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(plain) != "GROUP BY [region], [tier]" {
		t.Errorf("a plain grouping gained a set form: %s", plain)
	}

	// No grouping columns: no clause. An empty GROUP BY does not parse.
	if got, _ := mssql.GroupBy(&schema.Aggregate{Name: "X"}); got != "" {
		t.Errorf("an aggregation with no grouping emitted %q", got)
	}
}

// A rollup's subtotal rows belong above the detail they summarise. Ascending
// already sorts NULLs first here, so only a DESCENDING key needs a leading
// term — and that term is a CASE, because ISNULL() is MySQL's and means
// something else entirely in T-SQL (it is COALESCE with two arguments).
func TestSubtotalsSortAboveTheirDetail(t *testing.T) {
	desc, err := mssql.AggregateSuffix(agg(&schema.GroupingSets{Kind: schema.SetsRollup},
		[]schema.AggOrder{{As: "Region", Desc: true}}))
	if err != nil {
		t.Fatal(err)
	}
	// The alias is lowercased on the way out — pgsql.ColumnCase is shared, so a
	// declaration's Go name and its SQL alias cannot disagree between dialects.
	if !strings.Contains(desc, "CASE WHEN [region] IS NULL THEN 0 ELSE 1 END") {
		t.Errorf("a descending grouping key does not keep its subtotals first:\n%s", desc)
	}
	if strings.Contains(desc, "ISNULL(") {
		t.Errorf("ISNULL() is MySQL's; in T-SQL it is COALESCE and returns a value:\n%s", desc)
	}

	// Ascending needs nothing, and buying it a sort would be a cost with no
	// change in the answer.
	asc, err := mssql.AggregateSuffix(agg(&schema.GroupingSets{Kind: schema.SetsRollup},
		[]schema.AggOrder{{As: "Region"}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(asc, "CASE WHEN") {
		t.Errorf("an ascending key bought a sort it did not need:\n%s", asc)
	}

	// Every grouping column not already named is appended as a tiebreak: a
	// measure is not unique, and a top-N report is exactly the query that pages.
	one, err := mssql.AggregateSuffix(agg(nil, []schema.AggOrder{{As: "Total", Desc: true}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"[region]", "[tier]"} {
		if !strings.Contains(one, c) {
			t.Errorf("ordering by a measure alone does not page deterministically; "+
				"%s is missing:\n%s", c, one)
		}
	}

	// No explicit ordering: the grouping columns, by ALIAS — for a rollup the
	// expression is NULL in subtotal rows and the alias is what is in scope.
	none, err := mssql.AggregateSuffix(agg(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(none, "ORDER BY [region], [tier]") {
		t.Errorf("a grouped read with no ordering shuffles between requests:\n%s", none)
	}
}

// The SELECT list aliases every output, because the ORDER BY and the generated
// Row both name the alias rather than the expression.
func TestAggregateSelectAliasesEveryOutput(t *testing.T) {
	got, err := mssql.AggregateSelect("orders", agg(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[region] AS [region]", "[tier] AS [tier]",
		"sum([amt]) AS [total]", "FROM [orders]"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s:\n%s", want, got)
		}
	}
}

// A HAVING is rendered, and it follows the GROUP BY rather than preceding it.
func TestHavingFollowsTheGrouping(t *testing.T) {
	a := agg(nil, nil)
	cond := schema.Cond{Kind: schema.CondCmp, Op: ">",
		Left:  schema.Expr{Kind: schema.ExprAgg, Fn: "count", Args: []schema.Expr{{Kind: schema.ExprStar}}},
		Right: schema.Expr{Kind: schema.ExprLit, Lit: schema.Literal{Kind: "int8", I: 0}}}
	a.Having = &cond
	got, err := mssql.AggregateSuffix(a)
	if err != nil {
		t.Fatal(err)
	}
	g, h := strings.Index(got, "GROUP BY"), strings.Index(got, "HAVING")
	if g < 0 || h < 0 || g > h {
		t.Errorf("HAVING must follow GROUP BY:\n%s", got)
	}
}

func join(kind schema.JoinKind, where *schema.Cond, ctes []schema.CTE) *schema.Join {
	return &schema.Join{
		Name: "WithOrg",
		CTEs: ctes,
		Tables: []schema.JoinTable{{
			Kind: kind, Table: "orgs", Alias: "o",
			On: schema.Cond{Kind: schema.CondCmp, Op: "=",
				Left:  schema.Expr{Kind: schema.ExprCol, Tbl: "members", Col: "org_id"},
				Right: schema.Expr{Kind: schema.ExprCol, Tbl: "o", Col: "id"}},
		}},
		Select: []schema.JoinCol{
			{Expr: schema.Expr{Kind: schema.ExprCol, Tbl: "members", Col: "email"}, As: "Email"},
			{Expr: schema.Expr{Kind: schema.ExprCol, Tbl: "o", Col: "name"}, As: "OrgName"},
		},
		OrderBy: []schema.JoinOrder{
			{Expr: schema.Expr{Kind: schema.ExprCol, Tbl: "members", Col: "rank"}, Desc: true},
		},
		Where: where,
	}
}

// A joined table's soft-delete predicate goes in its ON clause, never the
// WHERE. In the WHERE, a parent whose only child is deleted is DROPPED and the
// outer join silently becomes an inner one.
func TestJoinedSoftDeleteGoesInTheOnClause(t *testing.T) {
	live := func(table, alias string) mssql.Live {
		return mssql.Live(mssql.LiveFor(alias, "deleted_at"))
	}
	got, err := mssql.JoinSelect("members", join(schema.JoinLeft, nil, nil),
		func(schema.CTE) (string, string) { return "", "" }, live)
	if err != nil {
		t.Fatal(err)
	}
	on := strings.Index(got, " ON ")
	pred := strings.Index(got, "[o].[deleted_at] IS NULL")
	if on < 0 || pred < 0 || pred < on {
		t.Errorf("the joined table's predicate is not in its ON clause:\n%s", got)
	}
	if !strings.Contains(got, "LEFT JOIN") {
		t.Errorf("a left join became an inner one:\n%s", got)
	}
}

// A CTE is emitted rather than refused: SQL Server has WITH and everything a
// declared aggregation can contain. compile/mysql refuses this outright.
func TestJoinMaterialisesACTE(t *testing.T) {
	ctes := []schema.CTE{{Alias: "spend", Table: "orders", Aggregate: "ByCustomer"}}
	got, err := mssql.JoinSelect("members", join(schema.JoinInner, nil, ctes),
		func(c schema.CTE) (string, string) {
			return "SELECT [customer_id], sum([total]) AS [Lifetime] FROM [orders]",
				" GROUP BY [customer_id]"
		}, nil)
	if err != nil {
		t.Fatalf("a CTE was refused; SQL Server has WITH: %v", err)
	}
	if !strings.HasPrefix(got, "WITH [spend] AS (SELECT ") {
		t.Errorf("the CTE is not materialised:\n%s", got)
	}
	if !strings.Contains(got, "GROUP BY [customer_id])") {
		t.Errorf("the CTE lost its grouping:\n%s", got)
	}
}

// The declared predicate and the driving table's soft delete compose, and the
// caller's predicates are ANDed onto them rather than replacing them.
func TestJoinDeclaredWhereComposes(t *testing.T) {
	cond := schema.Cond{Kind: schema.CondIsNotNull,
		Left: schema.Expr{Kind: schema.ExprCol, Tbl: "members", Col: "email"}}
	got, err := mssql.JoinDeclaredWhere(join(schema.JoinInner, &cond, nil),
		mssql.Live("[members].[deleted_at] IS NULL"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "[members].[email] IS NOT NULL") ||
		!strings.Contains(got, "[members].[deleted_at] IS NULL") {
		t.Errorf("one of the two predicates was dropped: %s", got)
	}

	// Only the driving predicate: no stray AND.
	only, err := mssql.JoinDeclaredWhere(join(schema.JoinInner, nil, nil),
		mssql.Live("[members].[deleted_at] IS NULL"))
	if err != nil {
		t.Fatal(err)
	}
	if only != "[members].[deleted_at] IS NULL" {
		t.Errorf("got %q", only)
	}

	// Neither: empty, not " AND ".
	if none, _ := mssql.JoinDeclaredWhere(join(schema.JoinInner, nil, nil), ""); none != "" {
		t.Errorf("got %q, want empty", none)
	}
}

// The declared ORDER BY is what the caller pages by, so it is emitted; no NULLS
// placement, which this server does not have.
func TestJoinSuffixOrders(t *testing.T) {
	got, err := mssql.JoinSuffix(join(schema.JoinInner, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got != " ORDER BY [members].[rank] DESC" {
		t.Errorf("got %q", got)
	}
	if none, _ := mssql.JoinSuffix(&schema.Join{Name: "X"}); none != "" {
		t.Errorf("an unordered join emitted %q", none)
	}
}

func union(distinct bool, order []schema.UnionOrder, params int) *schema.Union {
	u := &schema.Union{
		Name:     "Everything",
		Distinct: distinct,
		Cols:     []schema.UnionCol{{As: "Name"}},
		Branches: []schema.UnionBranch{
			{Table: "orgs", Exprs: []schema.Expr{{Kind: schema.ExprCol, Col: "name"}}},
			{Table: "members", Exprs: []schema.Expr{{Kind: schema.ExprCol, Col: "email"}}},
		},
		OrderBy: order,
	}
	for i := 0; i < params; i++ {
		u.Params = append(u.Params, schema.Param{Name: "P"})
	}
	return u
}

// A union's cap follows every declared parameter's ordinal, because nothing
// numbers this statement afterwards — and it needs an ORDER BY to hang from,
// which an undeclared ordering does not provide.
func TestUnionCapIsNumberedAndHasSomethingToHangFrom(t *testing.T) {
	two := mssql.UnionSuffix(union(false, nil, 2))
	if !strings.Contains(two, "FETCH NEXT @p3 ROWS ONLY") {
		t.Errorf("the cap does not follow the declared parameters: %s", two)
	}
	if !strings.Contains(two, "ORDER BY (SELECT NULL)") {
		t.Errorf("OFFSET/FETCH without an ORDER BY does not parse: %s", two)
	}

	ordered := mssql.UnionSuffix(union(false, []schema.UnionOrder{{Col: "Name", Desc: true}}, 0))
	if strings.Contains(ordered, "(SELECT NULL)") {
		t.Errorf("a declared ordering was replaced by the fallback: %s", ordered)
	}
	if !strings.Contains(ordered, "ORDER BY [name] DESC") {
		t.Errorf("the declared ordering was dropped: %s", ordered)
	}
	if !strings.Contains(ordered, "FETCH NEXT @p1 ROWS ONLY") {
		t.Errorf("with no parameters the cap is the first: %s", ordered)
	}
}

// UNION ALL by default: silently de-duplicating a caller's rows is a wrong
// answer, not a tidier one.
func TestUnionKeepsDuplicatesUnlessAsked(t *testing.T) {
	all, err := mssql.UnionSelect(union(false, nil, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "UNION ALL") {
		t.Errorf("the default de-duplicates:\n%s", all)
	}
	if !strings.Contains(all, "[orgs]") || !strings.Contains(all, "[members]") {
		t.Errorf("branches are not bracketed:\n%s", all)
	}
	distinct, err := mssql.UnionSelect(union(true, nil, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(distinct, "UNION ALL") {
		t.Errorf("a distinct union kept ALL:\n%s", distinct)
	}
}

// Per BRANCH, not per union: the branches read different tables and only some
// of them may soft-delete, so a predicate hoisted to the whole union would name
// a column half of them do not have.
func TestUnionAppliesSoftDeletePerBranch(t *testing.T) {
	got, err := mssql.UnionSelect(union(false, nil, 0), func(table string) mssql.Live {
		if table == "members" {
			return mssql.Live(mssql.LiveFor("", "deleted_at"))
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(got, "[deleted_at] IS NULL"); n != 1 {
		t.Errorf("the predicate reached %d branches, want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "FROM [members] WHERE [deleted_at] IS NULL") {
		t.Errorf("the branch that soft-deletes did not get its predicate:\n%s", got)
	}
}

// A NULLS placement on a union's ordering is refused before it is emitted: the
// ordering is the caller's paging order, so dropping the placement pages
// differently rather than spelling differently.
func TestUnionNullsPlacementIsRefused(t *testing.T) {
	first := true
	u := union(false, []schema.UnionOrder{{Col: "Name", NullsFirst: &first}}, 0)
	if err := mssql.UnionOrderRefused(u); err == nil {
		t.Fatal("a NULLS placement was accepted")
	}
	if err := mssql.UnionOrderRefused(union(false, nil, 0)); err != nil {
		t.Fatalf("a union with no placement was refused: %v", err)
	}
}

// Conditions: every shape the IR has, since a wrong keyword here is valid SQL
// that means something else.
func TestConditionsRender(t *testing.T) {
	col := func(c string) schema.Expr { return schema.Expr{Kind: schema.ExprCol, Col: c} }
	for _, c := range []struct {
		cond schema.Cond
		want string
	}{
		{schema.Cond{Kind: schema.CondIsNull, Left: col("a")}, "[a] IS NULL"},
		{schema.Cond{Kind: schema.CondIsNotNull, Left: col("a")}, "[a] IS NOT NULL"},
		{schema.Cond{Kind: schema.CondNot, Args: []schema.Cond{
			{Kind: schema.CondIsNull, Left: col("a")}}}, "NOT ([a] IS NULL)"},
		{schema.Cond{Kind: schema.CondAnd, Args: []schema.Cond{
			{Kind: schema.CondIsNull, Left: col("a")},
			{Kind: schema.CondIsNull, Left: col("b")}}},
			"([a] IS NULL AND [b] IS NULL)"},
		{schema.Cond{Kind: schema.CondOr, Args: []schema.Cond{
			{Kind: schema.CondIsNull, Left: col("a")},
			{Kind: schema.CondIsNull, Left: col("b")}}},
			"([a] IS NULL OR [b] IS NULL)"},
	} {
		got, err := mssql.Cond(c.cond)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}

// A window's frame and partition, which a declared aggregation ranks with.
func TestWindowRenders(t *testing.T) {
	col := func(c string) schema.Expr { return schema.Expr{Kind: schema.ExprCol, Col: c} }
	frame := schema.Frame{Kind: schema.FrameRows,
		Start: schema.FrameBound{Kind: schema.UnboundedPreceding},
		End:   schema.FrameBound{Kind: schema.CurrentRow}}
	win := schema.Window{
		PartitionBy: []schema.Expr{col("region")},
		OrderBy:     []schema.OrderTerm{{Expr: col("amt"), Desc: true}},
		Frame:       &frame,
	}
	got, err := mssql.Expr(schema.Expr{Kind: schema.ExprWindow, Fn: "row_number",
		Over: &win})
	if err != nil {
		t.Fatal(err)
	}
	want := "row_number() OVER (PARTITION BY [region] ORDER BY [amt] DESC " +
		"ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	// A NULLS placement inside OVER() decides which rows the frame covers, so
	// dropping it changes the result rather than its spelling.
	first := true
	win.OrderBy[0].NullsFirst = &first
	if _, err := mssql.Expr(schema.Expr{Kind: schema.ExprWindow, Fn: "row_number",
		Over: &win}); err == nil {
		t.Fatal("a window NULLS placement was accepted")
	}
}

// GROUPING() is real here — it is what tells a subtotal row from a detail row
// carrying a NULL — where MySQL has neither it nor the sets it belongs to.
func TestGroupingFunctionIsEmitted(t *testing.T) {
	got, err := mssql.Expr(schema.Expr{Kind: schema.ExprGrouping,
		Args: []schema.Expr{{Kind: schema.ExprCol, Col: "region"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "GROUPING([region])" {
		t.Errorf("got %s", got)
	}
}
