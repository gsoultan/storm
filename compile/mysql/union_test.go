package mysql_test

// The union, exists and expression writers, which nothing else reaches.
//
// The generated end-to-end covers base reads and writes; the shell gate
// PREPAREs the fragments it lists. Neither touches these, so a wrong keyword
// here would produce valid SQL that means something else and no gate would
// notice — which is exactly the case compile/mysql has a coverage floor for.

import (
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/mysql"
	"github.com/gsoultan/storm/schema"
)

func union(distinct bool, order []schema.UnionOrder) *schema.Union {
	return &schema.Union{
		Name:     "Everything",
		Distinct: distinct,
		Cols:     []schema.UnionCol{{As: "Name"}},
		Branches: []schema.UnionBranch{
			{Table: "orgs", Exprs: []schema.Expr{{Kind: schema.ExprCol, Col: "name"}}},
			{Table: "members", Exprs: []schema.Expr{{Kind: schema.ExprCol, Col: "email"}}},
		},
		OrderBy: order,
	}
}

func noLive(string) mysql.Live { return "" }

func TestUnionBranchesAreBacktickedAndSeparatedByTheRightKeyword(t *testing.T) {
	all, err := mysql.UnionSelect(union(false, nil), noLive)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "`orgs`") || !strings.Contains(all, "`members`") {
		t.Errorf("branches are not backticked:\n%s", all)
	}
	// UNION ALL, not UNION: the default keeps duplicates, and silently
	// de-duplicating a caller's rows is a wrong answer, not a tidier one.
	if !strings.Contains(all, "UNION ALL") {
		t.Errorf("the default union de-duplicates:\n%s", all)
	}

	distinct, err := mysql.UnionSelect(union(true, nil), noLive)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(distinct, "UNION ALL") {
		t.Errorf("a distinct union kept ALL:\n%s", distinct)
	}
}

// A union's soft-delete predicate is per BRANCH: the branches read different
// tables and only some may soft-delete, so one hoisted to the whole union would
// name a column half of them do not have.
func TestUnionAppliesSoftDeletePerBranch(t *testing.T) {
	got, err := mysql.UnionSelect(union(false, nil), func(tb string) mysql.Live {
		if tb == "members" {
			return mysql.Live(mysql.LiveFor("members", "deleted_at"))
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(got, "UNION")
	orgs, members := got[:i], got[i:]
	if strings.Contains(orgs, "deleted_at") {
		t.Errorf("a branch that does not soft-delete was filtered:\n%s", orgs)
	}
	if !strings.Contains(members, "deleted_at") {
		t.Errorf("a branch that does soft-delete was not filtered:\n%s", members)
	}
}

func TestUnionSuffixOrdersByOutputAliasAndCaps(t *testing.T) {
	got := mysql.UnionSuffix(union(false, []schema.UnionOrder{{Col: "Name", Desc: true}}))
	// The alias, not the branch's column: by the time ORDER BY applies, the
	// branch tables are gone and the output alias is the only thing in scope.
	if !strings.Contains(got, "ORDER BY `name` DESC") {
		t.Errorf("ordering = %q", got)
	}
	if !strings.HasSuffix(got, " LIMIT ?") {
		t.Errorf("the cap is not the last placeholder: %q", got)
	}
	if plain := mysql.UnionSuffix(union(false, nil)); plain != " LIMIT ?" {
		t.Errorf("an unordered union = %q, want just the cap", plain)
	}
}

// MySQL has no NULLS FIRST/LAST, and a union's ordering is the caller's paging
// order — dropping the placement would page differently, not just spell
// differently.
func TestUnionOrderingRefusesANullsPlacement(t *testing.T) {
	if err := mysql.UnionOrderRefused(union(false, []schema.UnionOrder{{Col: "Name"}})); err != nil {
		t.Errorf("a plain ordering was refused: %v", err)
	}
	first := true
	err := mysql.UnionOrderRefused(union(false, []schema.UnionOrder{{Col: "Name", NullsFirst: &first}}))
	if !errors.Is(err, mysql.ErrNoNullsPlacement) {
		t.Errorf("err = %v, want ErrNoNullsPlacement", err)
	}
}

// The inner table is ALWAYS aliased: a self-referential relation correlates a
// table with itself, and without the alias the inner reference captures the
// outer one and the predicate silently means something else.
func TestExistsAliasesTheInnerTable(t *testing.T) {
	got := mysql.ExistsFrag("comments", "post_id", "posts", "id", "")
	for _, want := range []string{"EXISTS (SELECT 1 FROM `comments` AS `_storm_e`",
		"`_storm_e`.`post_id` = `posts`.`id`"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, ")") {
		t.Errorf("unclosed: %s", got)
	}

	// Self-referential: the outer and inner names are the same table, which is
	// the case the alias exists for.
	self := mysql.ExistsFrag("nodes", "parent_id", "nodes", "id", "")
	if !strings.Contains(self, "`_storm_e`.`parent_id` = `nodes`.`id`") {
		t.Errorf("a self-referential exists lost its alias:\n%s", self)
	}
}

// "This parent has a related row" must not be satisfied by a row the child's
// own package would refuse to return.
func TestExistsCarriesTheChildsSoftDeletePredicate(t *testing.T) {
	got := mysql.ExistsFrag("comments", "post_id", "posts", "id",
		mysql.Live(mysql.LiveFor("comments", "deleted_at")))
	if !strings.Contains(got, "`_storm_e`.`deleted_at` IS NULL") {
		t.Errorf("the child predicate is not re-qualified to the alias:\n%s", got)
	}
	if strings.Contains(got, "`comments`.`deleted_at`") {
		t.Errorf("the child predicate still names the table, not the alias:\n%s", got)
	}
}

func TestNotExistsNegatesTheSameFragment(t *testing.T) {
	yes := mysql.ExistsFrag("comments", "post_id", "posts", "id", "")
	no := mysql.NotExistsFrag("comments", "post_id", "posts", "id", "")
	if no != "NOT "+yes {
		t.Errorf("the negated form is not the same fragment:\n%s\n%s", yes, no)
	}
}

// The open forms exist so the splicer can append wrapped child predicates. They
// have to be the closed form minus its paren, or the two drift.
func TestExistsOpenIsTheClosedFormWithoutItsParen(t *testing.T) {
	closed := mysql.ExistsFrag("comments", "post_id", "posts", "id", "")
	open := mysql.ExistsOpen("comments", "post_id", "posts", "id", "")
	if open+")" != closed {
		t.Errorf("open form drifted:\n%s\n%s", open, closed)
	}
	if mysql.NotExistsOpen("comments", "post_id", "posts", "id", "") != "NOT "+open {
		t.Error("the negated open form drifted")
	}
}

func TestOrderSuffixAndDefaultOrdering(t *testing.T) {
	if got := mysql.OrderSuffix("`id`"); got != " ORDER BY `id` LIMIT ?" {
		t.Errorf("OrderSuffix = %q", got)
	}
	// A read without ORDER BY has no defined order, and paging one is a bug
	// waiting for a plan change — so the primary key is the default.
	if got := mysql.DefaultOrderBy([]string{"org_id", "id"}, "id"); got != "`org_id`, `id`" {
		t.Errorf("DefaultOrderBy = %q", got)
	}
	if got := mysql.DefaultOrderBy(nil, "id"); got != "`id`" {
		t.Errorf("DefaultOrderBy with no primary key = %q", got)
	}
}

// A version bump reads the column's own value, so two concurrent writers cannot
// both land on the same next version.
func TestBumpReadsTheColumnItWrites(t *testing.T) {
	a, b := mysql.BumpFrag("version")
	if a != "`version` = `version` + 1" || b != "" {
		t.Errorf("BumpFrag = %q, %q", a, b)
	}
}

func TestExprAndCondRenderMySQLSpelling(t *testing.T) {
	got, err := mysql.Expr(schema.Expr{Kind: schema.ExprCol, Col: "name"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "`name`" {
		t.Errorf("Expr(col) = %q", got)
	}

	// An arithmetic expression, to reach the binary writer. The IR names the
	// operator abstractly and the back end spells it — `/` does not mean the
	// same thing in every dialect, which is why the character is not in the IR.
	sum := schema.Expr{
		Kind: schema.ExprBinary, Arith: schema.ArithAdd,
		Args: []schema.Expr{
			{Kind: schema.ExprCol, Col: "a"},
			{Kind: schema.ExprCol, Col: "b"},
		},
	}
	got, err = mysql.Expr(sum)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "`a`") || !strings.Contains(got, "`b`") || !strings.Contains(got, "+") {
		t.Errorf("Expr(arith) = %q", got)
	}

	c, err := mysql.Cond(schema.Cond{
		Kind:  schema.CondCmp,
		Op:    schema.OpEq,
		Left:  schema.Expr{Kind: schema.ExprCol, Col: "status"},
		Right: schema.Expr{Kind: schema.ExprLit, Lit: schema.Literal{Kind: "text", S: "live"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c, "`status`") || !strings.Contains(c, "=") {
		t.Errorf("Cond = %q", c)
	}
}
