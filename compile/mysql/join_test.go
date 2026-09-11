package mysql_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/mysql"
	"github.com/gsoultan/storm/schema"
)

func jcol(tbl, c string) schema.Expr {
	return schema.Expr{Kind: schema.ExprCol, Tbl: tbl, Col: c}
}

func memberJoin(kind schema.JoinKind) *schema.Join {
	return &schema.Join{
		Name: "OrgMembers",
		Tables: []schema.JoinTable{{
			Kind: kind, Table: "members", Alias: "members",
			On: schema.Cond{Kind: schema.CondCmp, Op: schema.OpEq,
				Left: jcol("members", "org_id"), Right: jcol("orgs", "id")},
		}},
		Select: []schema.JoinCol{{Expr: jcol("orgs", "name"), As: "OrgName"}},
	}
}

func memberLive(table, alias string) mysql.Live {
	if table != "members" {
		return ""
	}
	return mysql.Live(mysql.LiveFor(alias, "deleted_at"))
}

// The placement that would be silently wrong. Verified against 8.4.11: an org
// whose ONLY member is deleted still appears, NULL-extended. In the WHERE it
// would vanish, and the LEFT JOIN would have quietly become an inner one.
func TestJoinedTablePredicateGoesInTheOnClause(t *testing.T) {
	got, err := mysql.JoinSelect("orgs", memberJoin(schema.JoinLeft), nil, memberLive)
	if err != nil {
		t.Fatal(err)
	}
	on := got[strings.Index(got, " ON "):]
	if !strings.Contains(on, "`members`.`deleted_at` IS NULL") {
		t.Errorf("the child predicate is not in the ON clause:\n%s", got)
	}
	if strings.Contains(got, " WHERE ") {
		t.Errorf("a joined table's predicate reached the WHERE — an outer join filtered "+
			"there is an inner join:\n%s", got)
	}
	// Qualified. Unqualified is ambiguous the moment both tables have the
	// column, and wrong when only one does.
	if strings.Contains(got, " `deleted_at` IS NULL") {
		t.Errorf("the predicate is not qualified by alias:\n%s", got)
	}
}

// The driving table has no ON of its own.
func TestDrivingTablePredicateGoesInTheWhere(t *testing.T) {
	j := memberJoin(schema.JoinInner)
	w, err := mysql.JoinDeclaredWhere(j, memberLive("members", "members"))
	if err != nil {
		t.Fatal(err)
	}
	if w != "`members`.`deleted_at` IS NULL" {
		t.Errorf("driving-table predicate is %q", w)
	}
	// It composes with a declared filter rather than replacing it.
	j.Where = &schema.Cond{Kind: schema.CondIsNull, Left: jcol("orgs", "closed_at")}
	w, err = mysql.JoinDeclaredWhere(j, memberLive("members", "members"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w, "closed_at") || !strings.Contains(w, "deleted_at") {
		t.Errorf("the declared filter and the predicate did not compose: %q", w)
	}
}

// Everything is backticked and bare-placeholdered, like the rest of the dialect.
func TestJoinCarriesMySQLSpelling(t *testing.T) {
	got, err := mysql.JoinSelect("orgs", memberJoin(schema.JoinLeft), nil, memberLive)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, `"`) {
		t.Errorf("a double-quoted identifier reached MySQL SQL:\n%s", got)
	}
	if strings.Contains(got, "$") {
		t.Errorf("a PostgreSQL placeholder reached MySQL SQL:\n%s", got)
	}
}

// A join may materialise a declared aggregation as a CTE. MySQL 8 HAS WITH —
// but not the FILTER and GROUPING SETS forms an aggregation may carry, and
// aggregates have no MySQL lowering yet. Refused rather than half-emitted.
func TestJoinWithACTEIsRefused(t *testing.T) {
	j := memberJoin(schema.JoinLeft)
	j.CTEs = []schema.CTE{{Alias: "totals", Table: "members", Aggregate: "ByOrg"}}
	_, err := mysql.JoinSelect("orgs", j, nil, memberLive)
	if !errors.Is(err, mysql.ErrCTEUnavailable) {
		t.Fatalf("a join with a CTE returned %v; it must refuse until aggregates lower", err)
	}
}

// A declared ordering is what the caller pages by, so an explicit NULLS
// placement cannot be dropped — MySQL has none, and dropping it would page
// differently rather than spell differently.
func TestJoinOrderingRefusesANullsPlacement(t *testing.T) {
	j := memberJoin(schema.JoinInner)
	j.OrderBy = []schema.JoinOrder{{Expr: jcol("orgs", "name")}}
	if _, err := mysql.JoinSuffix(j); err != nil {
		t.Fatalf("a plain ordering was refused: %v", err)
	}
	got, _ := mysql.JoinSuffix(j)
	if strings.Contains(strings.ToUpper(got), "NULLS") {
		t.Errorf("a NULLS placement reached MySQL SQL: %s", got)
	}
}
