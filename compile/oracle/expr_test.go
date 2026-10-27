package oracle_test

// The expression writer and the fixed punctuation, without a server. Most of
// this is also run by internal/oraclespike, against a real Oracle, but that is
// a separate module whose coverage does not count here; what is pinned below is
// the TEXT, so a change to a spelling shows up in this package's own tests.

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/oracle"
	"github.com/gsoultan/storm/schema"
)

// Every expression node the IR has. A node with no case falls through to an
// empty string, which is SQL that means something else.
func TestExpressionNodesRender(t *testing.T) {
	amt := col("amt")
	for _, c := range []struct {
		e    schema.Expr
		want string
	}{
		{schema.Expr{Kind: schema.ExprCol, Tbl: "o", Col: "id"}, `"o"."id"`},
		{schema.Expr{Kind: schema.ExprStar}, "*"},
		{schema.Expr{Kind: schema.ExprLit,
			Lit: schema.Literal{Kind: schema.TypeText, S: "it's"}}, `'it''s'`},
		{schema.Expr{Kind: schema.ExprParam, Param: 2}, ":3"},
		{schema.Expr{Kind: schema.ExprFunc, Fn: "coalesce",
			Args: []schema.Expr{amt, amt}}, `coalesce("amt", "amt")`},
		{schema.Expr{Kind: schema.ExprAgg, Fn: "count", Distinct: true,
			Args: []schema.Expr{amt}}, `count(DISTINCT "amt")`},
		{schema.Expr{Kind: schema.ExprGrouping,
			Args: []schema.Expr{col("region")}}, `GROUPING("region")`},
		{schema.Expr{Kind: schema.ExprBinary, Arith: schema.ArithAdd,
			Args: []schema.Expr{amt, amt}}, `("amt" + "amt")`},
		{schema.Expr{Kind: schema.ExprBinary, Arith: schema.ArithSub,
			Args: []schema.Expr{amt, amt}}, `("amt" - "amt")`},
		{schema.Expr{Kind: schema.ExprBinary, Arith: schema.ArithMul,
			Args: []schema.Expr{amt, amt}}, `("amt" * "amt")`},
	} {
		got, err := oracle.Expr(c.e)
		if err != nil {
			t.Errorf("%v: %v", c.e.Kind, err)
			continue
		}
		if got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}

	// A binary node with the wrong arity renders nothing rather than half an
	// expression.
	if got, _ := oracle.Expr(schema.Expr{Kind: schema.ExprBinary,
		Arith: schema.ArithAdd, Args: []schema.Expr{amt}}); got != "" {
		t.Errorf("a malformed binary node rendered %q", got)
	}
}

// Oracle's NUMBER division keeps the fraction, so there is no CAST — only the
// ROUND to the scale the declaration asked for.
func TestDivisionRoundsWithoutACast(t *testing.T) {
	got, err := oracle.Expr(schema.Expr{Kind: schema.ExprBinary, Arith: schema.ArithDiv,
		Type: schema.Type{Name: schema.TypeNumeric, Scale: 2},
		Args: []schema.Expr{col("total"), col("count")}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `ROUND("total" / "count", 2)`; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// An unknown operator is refused rather than falling through to a token that
// means something else.
func TestUnknownArithmeticIsRefused(t *testing.T) {
	if _, err := oracle.Expr(schema.Expr{Kind: schema.ExprBinary,
		Arith: schema.ArithOp("nope"),
		Args:  []schema.Expr{col("a"), col("b")}}); err == nil {
		t.Fatal("an unknown operator was lowered")
	}
}

// FILTER has no clause here and the rewrite changes what count(*) counts.
func TestFilterIsRefused(t *testing.T) {
	cond := schema.Cond{Kind: schema.CondIsNotNull, Left: col("paid_at")}
	_, err := oracle.Expr(schema.Expr{Kind: schema.ExprAgg, Fn: "count",
		Args: []schema.Expr{{Kind: schema.ExprStar}}, Filter: &cond})
	if err != oracle.ErrNoFilterClause {
		t.Fatalf("err = %v, want ErrNoFilterClause", err)
	}
}

// Conditions: every shape the IR has, since a wrong keyword here is valid SQL
// that means something else.
func TestConditionsRender(t *testing.T) {
	isNull := func(c string) schema.Cond { return schema.Cond{Kind: schema.CondIsNull, Left: col(c)} }
	for _, c := range []struct {
		cond schema.Cond
		want string
	}{
		{schema.Cond{Kind: schema.CondCmp, Op: schema.OpGte, Left: col("a"),
			Right: schema.Expr{Kind: schema.ExprParam}}, `"a" >= :1`},
		{isNull("a"), `"a" IS NULL`},
		{schema.Cond{Kind: schema.CondIsNotNull, Left: col("a")}, `"a" IS NOT NULL`},
		{schema.Cond{Kind: schema.CondNot, Args: []schema.Cond{isNull("a")}}, `NOT ("a" IS NULL)`},
		{schema.Cond{Kind: schema.CondAnd, Args: []schema.Cond{isNull("a"), isNull("b")}},
			`("a" IS NULL AND "b" IS NULL)`},
		{schema.Cond{Kind: schema.CondOr, Args: []schema.Cond{isNull("a"), isNull("b")}},
			`("a" IS NULL OR "b" IS NULL)`},
	} {
		got, err := oracle.Cond(c.cond)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}

// NULLS placement inside OVER() is SPELLED here, where SQL Server has to refuse
// it: Oracle has it everywhere an ORDER BY appears.
func TestWindowSpellsItsNullPlacement(t *testing.T) {
	first, last := true, false
	frame := schema.Frame{Kind: schema.FrameRows,
		Start: schema.FrameBound{Kind: schema.UnboundedPreceding},
		End:   schema.FrameBound{Kind: schema.CurrentRow}}
	win := schema.Window{
		PartitionBy: []schema.Expr{col("region")},
		OrderBy: []schema.OrderTerm{
			{Expr: col("amt"), Desc: true, NullsFirst: &last},
			{Expr: col("id"), NullsFirst: &first},
		},
		Frame: &frame,
	}
	got, err := oracle.Expr(schema.Expr{Kind: schema.ExprWindow, Fn: "row_number", Over: &win})
	if err != nil {
		t.Fatal(err)
	}
	want := `row_number() OVER (PARTITION BY "region" ORDER BY "amt" DESC NULLS LAST, ` +
		`"id" NULLS FIRST ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	// An aggregate can be windowed too, and a window with only an ordering
	// has no stray space where the partition would be.
	ord := schema.Window{OrderBy: []schema.OrderTerm{{Expr: col("amt")}}}
	got, err = oracle.Expr(schema.Expr{Kind: schema.ExprAgg, Fn: "sum",
		Args: []schema.Expr{col("amt")}, Over: &ord})
	if err != nil {
		t.Fatal(err)
	}
	if want := `sum("amt") OVER (ORDER BY "amt")`; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// Every frame bound, since a wrong one silently changes which rows the
// aggregate reads — and a numeric RANGE offset is refused, because whether it
// is legal depends on a column type the declaration does not pin.
func TestFrameBounds(t *testing.T) {
	window := func(f schema.Frame) (string, error) {
		win := schema.Window{Frame: &f}
		return oracle.Expr(schema.Expr{Kind: schema.ExprWindow, Fn: "sum",
			Args: []schema.Expr{col("amt")}, Over: &win})
	}
	for _, c := range []struct {
		start, end schema.FrameBound
		want       string
	}{
		{schema.FrameBound{Kind: schema.UnboundedPreceding},
			schema.FrameBound{Kind: schema.UnboundedFollowing},
			"ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING"},
		{schema.FrameBound{Kind: schema.Preceding, N: 2},
			schema.FrameBound{Kind: schema.Following, N: 3},
			"ROWS BETWEEN 2 PRECEDING AND 3 FOLLOWING"},
	} {
		got, err := window(schema.Frame{Kind: schema.FrameRows, Start: c.start, End: c.end})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("got  %s\nwant it to contain %s", got, c.want)
		}
	}

	got, err := window(schema.Frame{Kind: schema.FrameRange,
		Start: schema.FrameBound{Kind: schema.UnboundedPreceding},
		End:   schema.FrameBound{Kind: schema.CurrentRow}})
	if err != nil {
		t.Fatalf("an unbounded RANGE frame was refused: %v", err)
	}
	if !strings.Contains(got, "RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW") {
		t.Errorf("got %s", got)
	}

	if _, err := window(schema.Frame{Kind: schema.FrameRange,
		Start: schema.FrameBound{Kind: schema.Preceding, N: 3},
		End:   schema.FrameBound{Kind: schema.CurrentRow}}); err != oracle.ErrNoRangeOffset {
		t.Fatalf("err = %v, want ErrNoRangeOffset", err)
	}
}

// The upsert's punctuation. Two things here are ORA errors when wrong: an AS
// before a table alias, and an ON condition without its parentheses.
func TestMergePunctuation(t *testing.T) {
	p := oracle.Merge("users")
	if !strings.HasPrefix(p.Into, `MERGE INTO "users" "_storm_m" USING (VALUES (`) {
		t.Errorf("Into = %q", p.Into)
	}
	if strings.Contains(p.Into, " AS ") {
		t.Errorf("a table alias by AS is ORA-00933: %q", p.Into)
	}
	if p.OnLead != ") ON (" || p.OnClose != ")" {
		t.Errorf("the ON condition must be parenthesised (ORA-00969): %q ... %q",
			p.OnLead, p.OnClose)
	}
	if p.Tgt == p.Src {
		t.Errorf("the target and the source share an alias: %q", p.Tgt)
	}
	if p.End != "" {
		t.Errorf("a trailing ; is ORA-00911 through the protocol: %q", p.End)
	}
	if got := oracle.MergeOutput([]string{"id"}); got != "" {
		t.Errorf("MergeOutput = %q; the executor has no OUT parameter to bind", got)
	}
}

// The declared predicate and the driving table's soft delete compose, and
// neither is dropped when the other is absent.
func TestJoinDeclaredWhereComposes(t *testing.T) {
	cond := schema.Cond{Kind: schema.CondIsNotNull,
		Left: schema.Expr{Kind: schema.ExprCol, Tbl: "members", Col: "email"}}
	live := oracle.Live(oracle.LiveFor("members", "deleted_at"))

	both, err := oracle.JoinDeclaredWhere(&schema.Join{Where: &cond}, live)
	if err != nil {
		t.Fatal(err)
	}
	if want := `("members"."email" IS NOT NULL) AND "members"."deleted_at" IS NULL`; both != want {
		t.Errorf("got  %s\nwant %s", both, want)
	}
	if only, _ := oracle.JoinDeclaredWhere(&schema.Join{}, live); only != string(live) {
		t.Errorf("got %q", only)
	}
	if mine, _ := oracle.JoinDeclaredWhere(&schema.Join{Where: &cond}, ""); mine != `"members"."email" IS NOT NULL` {
		t.Errorf("got %q", mine)
	}
}

// The declared ORDER BY is what the caller pages by, so it is emitted as
// declared; an unordered join emits nothing.
func TestJoinSuffixOrders(t *testing.T) {
	j := &schema.Join{OrderBy: []schema.JoinOrder{
		{Expr: schema.Expr{Kind: schema.ExprCol, Tbl: "m", Col: "rank"}, Desc: true},
		{Expr: schema.Expr{Kind: schema.ExprCol, Tbl: "m", Col: "id"}},
	}}
	got, err := oracle.JoinSuffix(j)
	if err != nil {
		t.Fatal(err)
	}
	if want := ` ORDER BY "m"."rank" DESC, "m"."id"`; got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if none, _ := oracle.JoinSuffix(&schema.Join{}); none != "" {
		t.Errorf("an unordered join emitted %q", none)
	}
}

// The plain read shapes. Exists is capped by the ordinary suffix — FETCH FIRST
// needs no ORDER BY here, unlike SQL Server's OFFSET/FETCH.
func TestReadPrefixesAndDefaultOrder(t *testing.T) {
	if got := oracle.SelectPrefix("users", []string{"id", "email"}); got != `SELECT "id", "email" FROM "users"` {
		t.Errorf("SelectPrefix = %s", got)
	}
	if got := oracle.ExistsPrefix("users") + oracle.ExistsSuffix(); got != `SELECT 1 FROM "users" FETCH FIRST 1 ROWS ONLY` {
		t.Errorf("exists = %s", got)
	}
	if got := oracle.DefaultOrderBy([]string{"tenant_id", "id"}, "rowid"); got != `"tenant_id", "id"` {
		t.Errorf("DefaultOrderBy = %s", got)
	}
	if got := oracle.DefaultOrderBy(nil, "created_at"); got != `"created_at"` {
		t.Errorf("DefaultOrderBy without a key = %s", got)
	}
	if oracle.RowCmpOp(0) != " > " || oracle.RowCmpOp(1) != " < " {
		t.Error("a keyset comparison must be strict in both directions")
	}
}

// The lock is a suffix here, never a hint, and the doc text names the two
// refusals an adopter will meet.
func TestLockIsASuffixAndItsNotesNameTheRefusals(t *testing.T) {
	for m := 0; m < oracle.NumLockModes; m++ {
		if h := oracle.LockHint(m); h != "" {
			t.Errorf("LockHint(%d) = %q; Oracle locks by suffix", m, h)
		}
	}
	notes := strings.Join(oracle.LockNotes(), "\n")
	for _, want := range []string{"no shared row", "ORA-02014", "ROWNUM"} {
		if !strings.Contains(notes, want) {
			t.Errorf("LockNotes does not mention %q", want)
		}
	}
}

// The write-path pieces that are fixed text.
func TestWriteFragments(t *testing.T) {
	if got := oracle.InsertPrefix("users"); got != `INSERT INTO "users"` {
		t.Errorf("InsertPrefix = %s", got)
	}
	if got := oracle.DeletePrefix("users"); got != `DELETE FROM "users"` {
		t.Errorf("DeletePrefix = %s", got)
	}
	if a, _ := oracle.SetFrag("email"); a != `"email" = :` {
		t.Errorf("SetFrag = %s", a)
	}
	if a, _ := oracle.BumpFrag("version"); a != `"version" = "version" + 1` {
		t.Errorf("BumpFrag = %s", a)
	}
	if a, _ := oracle.AddFrag("hits"); a != `"hits" = "hits" + :` {
		t.Errorf("AddFrag = %s", a)
	}
	if got := oracle.RestoreSet("users", "deleted_at"); got != `UPDATE "users" SET "deleted_at" = NULL` {
		t.Errorf("RestoreSet = %s", got)
	}
	if got := oracle.ReturningClause([]string{"id"}); got != "" {
		t.Errorf("ReturningClause = %q", got)
	}
}

// A semi-join's open form is its closed form without the final paren, and the
// negation is a prefix — so the two cannot drift.
func TestExistsOpenFormsMatchTheClosedOnes(t *testing.T) {
	live := oracle.Live(oracle.LiveFor("", "deleted_at"))
	closed := oracle.ExistsFrag("posts", "author_id", "authors", "id", live)
	if oracle.ExistsOpen("posts", "author_id", "authors", "id", live)+")" != closed {
		t.Error("ExistsOpen and ExistsFrag disagree")
	}
	if oracle.NotExistsOpen("posts", "author_id", "authors", "id", live)+")" != "NOT "+closed {
		t.Error("NotExistsOpen and ExistsFrag disagree")
	}
	if !strings.Contains(closed, `"_storm_x"."deleted_at" IS NULL`) {
		t.Errorf("the child predicate was not re-qualified: %s", closed)
	}
}

// Zero: Oracle's path overflow is an ERROR, not a silent truncation, so there
// is no depth past which the cycle guard quietly stops guarding.
func TestRecursionHasNoSilentDepthLimit(t *testing.T) {
	if d := oracle.MaxRecursionDepth("id"); d != 0 {
		t.Errorf("MaxRecursionDepth = %d", d)
	}
}
