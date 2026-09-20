package mssql_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/mssql"
	"github.com/gsoultan/storm/schema"
)

// The statement prefixes and the write fragments. They are one line each, which
// is exactly why they need a test: a missing bracket or a stray keyword here
// produces SQL that a reader skims past and a server rejects at run time.
func TestPrefixesAndFragments(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{mssql.SelectPrefix("users", []string{"id", "email"}),
			"SELECT [id], [email] FROM [users]"},
		{mssql.CountPrefix("users"), "SELECT count(*) FROM [users]"},
		{mssql.ExistsPrefix("users"), "SELECT TOP 1 1 FROM [users]"},
		{mssql.UpdatePrefix("users"), "UPDATE [users] SET "},
		{mssql.DeletePrefix("users"), "DELETE FROM [users]"},
		{mssql.InsertPrefix("users"), "INSERT INTO [users]"},
		{mssql.SoftDeleteWhere("deleted_at"), "[deleted_at] IS NULL"},
	} {
		if c.got != c.want {
			t.Errorf("got  %s\nwant %s", c.got, c.want)
		}
	}

	if a, b := mssql.SetFrag("email"); a != "[email] = @" || b != "" {
		t.Errorf("SetFrag = %q, %q", a, b)
	}
	// A version bump reads its own column rather than binding a value, which is
	// what makes it atomic against a concurrent writer.
	if a, b := mssql.BumpFrag("version"); a != "[version] = [version] + 1" || b != "" {
		t.Errorf("BumpFrag = %q, %q", a, b)
	}

	open, sep, mid, close := mssql.InsertParts(nil)
	if open != " (" || sep != ", " || mid != ") VALUES (" || close != ")" {
		t.Errorf("InsertParts = %q %q %q %q", open, sep, mid, close)
	}
}

// The default ordering is the primary key, because a read without ORDER BY has
// no defined order and paging one is a bug waiting for a plan change.
func TestDefaultOrderIsThePrimaryKey(t *testing.T) {
	if got := mssql.DefaultOrderBy([]string{"id"}, "x"); got != "[id]" {
		t.Errorf("got %s", got)
	}
	if got := mssql.DefaultOrderBy([]string{"org_id", "id"}, "x"); got != "[org_id], [id]" {
		t.Errorf("a composite key lost a column: %s", got)
	}
	// No declared key: the fallback the caller named, still quoted.
	if got := mssql.DefaultOrderBy(nil, "rowid"); got != "[rowid]" {
		t.Errorf("got %s", got)
	}
}

// Strict inequality only: >= returns the row you just showed.
func TestRowComparisonIsStrict(t *testing.T) {
	if got := mssql.RowCmpOp(0); got != " > " {
		t.Errorf("got %q", got)
	}
	if got := mssql.RowCmpOp(1); got != " < " {
		t.Errorf("got %q", got)
	}
	if !mssql.RowCmpExpand {
		t.Error("SQL Server has no row constructor; the splicer has to expand")
	}
}

// The plain operators, each of which has to bind exactly one value — or none,
// for the two that test nullness.
func TestOperatorFragments(t *testing.T) {
	for _, c := range []struct {
		op   string
		want string
	}{
		{"Eq", "[a] = @"},
		{"NotEq", "[a] <> @"},
		{"Gt", "[a] > @"},
		{"Gte", "[a] >= @"},
		{"Lt", "[a] < @"},
		{"Lte", "[a] <= @"},
		{"Like", "[a] LIKE @"},
		{"IsNull", "[a] IS NULL"},
		{"IsNotNull", "[a] IS NOT NULL"},
	} {
		a, b, ok := mssql.Frag(c.op, "[a]")
		if !ok {
			t.Errorf("%s has no lowering", c.op)
			continue
		}
		if a+b != c.want {
			t.Errorf("%s: got %s, want %s", c.op, a+b, c.want)
		}
	}

	// Case-insensitive LIKE names the collation rather than trusting the
	// database's: a server installed with a CS collation would otherwise make a
	// portable model stop being portable.
	a, _, _ := mssql.Frag("ILike", "[a]")
	if !strings.Contains(a, "COLLATE Latin1_General_CI_AS") {
		t.Errorf("ILike trusts the database's collation: %s", a)
	}

	// EqLower is the expression an index can be built on, on both sides.
	a, b, _ := mssql.Frag("EqLower", "[a]")
	if a+b != "LOWER([a]) = LOWER(@)" {
		t.Errorf("EqLower = %s", a+b)
	}

	// An operator this back end has never heard of is a generation error, not
	// an empty fragment that quietly matches every row.
	if _, _, ok := mssql.Frag("Overlaps", "[a]"); ok {
		t.Error("an unknown operator produced a fragment")
	}
	if mssql.Supported("Overlaps") {
		t.Error("an unknown operator reports supported")
	}
	if !mssql.Supported("In") || !mssql.Supported("NotIn") {
		t.Error("the list operators have a lowering and should report supported")
	}
}

// Every expression node the IR has. A node with no case falls through to an
// empty string, which is SQL that means something else.
func TestExpressionNodesRender(t *testing.T) {
	col := schema.Expr{Kind: schema.ExprCol, Col: "amt"}
	for _, c := range []struct {
		e    schema.Expr
		want string
	}{
		{schema.Expr{Kind: schema.ExprCol, Tbl: "o", Col: "id"}, "[o].[id]"},
		{schema.Expr{Kind: schema.ExprStar}, "*"},
		{schema.Expr{Kind: schema.ExprParam, Param: 2}, "@p3"},
		{schema.Expr{Kind: schema.ExprFunc, Fn: "coalesce",
			Args: []schema.Expr{col, col}}, "coalesce([amt], [amt])"},
		{schema.Expr{Kind: schema.ExprAgg, Fn: "count", Distinct: true,
			Args: []schema.Expr{col}}, "count(DISTINCT [amt])"},
		{schema.Expr{Kind: schema.ExprBinary, Arith: schema.ArithAdd,
			Args: []schema.Expr{col, col}}, "([amt] + [amt])"},
		{schema.Expr{Kind: schema.ExprBinary, Arith: schema.ArithSub,
			Args: []schema.Expr{col, col}}, "([amt] - [amt])"},
		{schema.Expr{Kind: schema.ExprBinary, Arith: schema.ArithMul,
			Args: []schema.Expr{col, col}}, "([amt] * [amt])"},
	} {
		got, err := mssql.Expr(c.e)
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
	if got, _ := mssql.Expr(schema.Expr{Kind: schema.ExprBinary,
		Arith: schema.ArithAdd, Args: []schema.Expr{col}}); got != "" {
		t.Errorf("a malformed binary node rendered %q", got)
	}
}

// Every frame bound, since a wrong one silently changes which rows the
// aggregate reads.
func TestFrameBounds(t *testing.T) {
	col := schema.Expr{Kind: schema.ExprCol, Col: "amt"}
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
		{schema.FrameBound{Kind: schema.CurrentRow},
			schema.FrameBound{Kind: schema.CurrentRow},
			"ROWS BETWEEN CURRENT ROW AND CURRENT ROW"},
	} {
		frame := schema.Frame{Kind: schema.FrameRows, Start: c.start, End: c.end}
		win := schema.Window{Frame: &frame}
		got, err := mssql.Expr(schema.Expr{Kind: schema.ExprWindow, Fn: "sum",
			Args: []schema.Expr{col}, Over: &win})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("got  %s\nwant it to contain %s", got, c.want)
		}
	}

	// A RANGE frame with only unbounded and current-row ends IS accepted: it is
	// the row OFFSET that SQL Server has no form for.
	frame := schema.Frame{Kind: schema.FrameRange,
		Start: schema.FrameBound{Kind: schema.UnboundedPreceding},
		End:   schema.FrameBound{Kind: schema.CurrentRow}}
	win := schema.Window{Frame: &frame}
	got, err := mssql.Expr(schema.Expr{Kind: schema.ExprWindow, Fn: "sum",
		Args: []schema.Expr{col}, Over: &win})
	if err != nil {
		t.Fatalf("an unbounded RANGE frame was refused: %v", err)
	}
	if !strings.Contains(got, "RANGE BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW") {
		t.Errorf("got %s", got)
	}

	// A window with only a partition, and one with only an ordering.
	part := schema.Window{PartitionBy: []schema.Expr{col}}
	if got, _ := mssql.Expr(schema.Expr{Kind: schema.ExprWindow, Fn: "count",
		Over: &part}); got != "count() OVER (PARTITION BY [amt])" {
		t.Errorf("got %s", got)
	}
	ord := schema.Window{OrderBy: []schema.OrderTerm{{Expr: col}}}
	if got, _ := mssql.Expr(schema.Expr{Kind: schema.ExprWindow, Fn: "count",
		Over: &ord}); got != "count() OVER (ORDER BY [amt])" {
		t.Errorf("got %s", got)
	}
}

// The Live carrier: a read builder that takes one cannot forget it by receiving
// "" out of habit.
func TestLiveCarrier(t *testing.T) {
	if !mssql.Live("").Empty() {
		t.Error("an empty predicate does not report empty")
	}
	if mssql.Live("[a] IS NULL").Empty() {
		t.Error("a real predicate reports empty")
	}
	var b strings.Builder
	b.WriteString("SELECT 1 FROM [t]")
	mssql.Live("[deleted_at] IS NULL").AndInto(&b, false)
	if got := b.String(); got != "SELECT 1 FROM [t] WHERE [deleted_at] IS NULL" {
		t.Errorf("got %s", got)
	}
	b.Reset()
	b.WriteString("SELECT 1 FROM [t] WHERE [a] = @p1")
	mssql.Live("[deleted_at] IS NULL").AndInto(&b, true)
	if !strings.HasSuffix(b.String(), " AND [deleted_at] IS NULL") {
		t.Errorf("got %s", b.String())
	}
	b.Reset()
	mssql.Live("").AndInto(&b, false)
	if b.String() != "" {
		t.Errorf("an empty predicate wrote %q", b.String())
	}
}
