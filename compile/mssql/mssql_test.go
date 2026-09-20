package mssql_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/mssql"
	"github.com/gsoultan/storm/schema"
)

// The four things that could not be expressed through the seam as M9 left it.
// Each is asserted here on the TEXT; internal/mssqlspike asserts that a server
// accepts it. Both, because a golden test proves the generator agrees with
// itself and a live one does not say which decision produced the bytes.

// Brackets, not double quotes: `"name"` is an identifier only under
// QUOTED_IDENTIFIER ON, and that is a per-connection setting.
func TestIdentIsBracketed(t *testing.T) {
	if got := mssql.Ident("order"); got != "[order]" {
		t.Errorf("Ident(order) = %s", got)
	}
	if got := mssql.Ident("we]rd"); got != "[we]]rd]" {
		t.Errorf("a bracket inside a name was not doubled: %s", got)
	}
}

// OFFSET is not optional — FETCH without it is a syntax error — so the uncapped
// form spells a literal zero rather than binding one.
func TestPagingNamesOffsetFirstAndAlwaysNamesIt(t *testing.T) {
	plain := mssql.LimitOffsetSuffix(false)
	if !strings.Contains(plain, "OFFSET 0 ROWS") {
		t.Errorf("FETCH without OFFSET does not parse: %s", plain)
	}
	if n := strings.Count(plain, mssql.Placeholder); n != 1 {
		t.Errorf("the uncapped form binds %d parameters, want 1: %s", n, plain)
	}

	paged := mssql.LimitOffsetSuffix(true)
	off := strings.Index(paged, "OFFSET")
	fetch := strings.Index(paged, "FETCH")
	if off < 0 || fetch < 0 || off > fetch {
		t.Errorf("OFFSET must precede FETCH: %s", paged)
	}
	if !mssql.PagingOffsetFirst {
		t.Error("the operands are reversed against LIMIT n OFFSET m, so the binder " +
			"has to be told; PagingOffsetFirst says so")
	}
}

// An existence probe caps with TOP, in the PREFIX — because the other cap,
// OFFSET/FETCH, requires an ORDER BY and a probe has none to give.
func TestExistsCapsWithTopNotFetch(t *testing.T) {
	if got := mssql.ExistsPrefix("users"); !strings.Contains(got, "TOP 1") {
		t.Errorf("no cap in the prefix: %s", got)
	}
	if got := mssql.ExistsSuffix(); got != "" {
		t.Errorf("ExistsSuffix = %q, want empty — the cap is in the prefix", got)
	}
}

// The lock is a TABLE HINT, so it goes after the table name and not at the end.
// FOR UPDATE does not exist in T-SQL at all.
func TestLocksAreTableHints(t *testing.T) {
	for m := 1; m < mssql.NumLockModes; m++ {
		hint := mssql.LockHint(m)
		if hint == "" {
			t.Errorf("lock mode %d has no hint", m)
			continue
		}
		if !strings.Contains(hint, "ROWLOCK") {
			t.Errorf("mode %d takes whatever granularity the server picks: %s", m, hint)
		}
		if strings.Contains(hint, "FOR UPDATE") || strings.Contains(hint, "FOR SHARE") {
			t.Errorf("mode %d emitted a clause T-SQL does not have: %s", m, hint)
		}
		if s := mssql.LockSuffix(m); s != "" {
			t.Errorf("mode %d put something at the end, where a hint cannot go: %q", m, s)
		}
	}
	// SKIP LOCKED is READPAST, and it is only honoured for row locks.
	if got := mssql.LockHint(mssql.LockUpdateSkipLocked); !strings.Contains(got, "READPAST") {
		t.Errorf("skip-locked is not READPAST: %s", got)
	}
	// FOR SHARE is REPEATABLEREAD, not HOLDLOCK: HOLDLOCK is SERIALIZABLE and
	// takes range locks, which refuses inserts the caller never asked to block.
	if got := mssql.LockHint(mssql.LockShare); strings.Contains(got, "HOLDLOCK") {
		t.Errorf("the shared lock was widened to SERIALIZABLE: %s", got)
	}
}

// OUTPUT sits between the column list and VALUES on an insert, which is what
// InsertParts.Mid is. At the end it is a syntax error.
func TestOutputIsPositionalOnInsert(t *testing.T) {
	_, _, mid, _ := mssql.InsertParts([]string{"id", "created_at"})
	if !strings.Contains(mid, "OUTPUT INSERTED.[id]") {
		t.Errorf("the OUTPUT clause is not in the punctuation between the columns "+
			"and VALUES: %q", mid)
	}
	if strings.Index(mid, "OUTPUT") > strings.Index(mid, "VALUES") {
		t.Errorf("OUTPUT after VALUES does not parse: %q", mid)
	}

	// No returning list, no clause — an OUTPUT with no columns does not parse
	// either.
	if _, _, bare, _ := mssql.InsertParts(nil); strings.Contains(bare, "OUTPUT") {
		t.Errorf("an empty returning list produced a clause: %q", bare)
	}

	full, err := mssql.InsertStmt("users", []string{"id", "email"}, []string{"id"})
	if err != nil {
		t.Fatal(err)
	}
	want := "INSERT INTO [users] ([id], [email]) OUTPUT INSERTED.[id] VALUES (@p1, @p2)"
	if full != want {
		t.Errorf("got  %s\nwant %s", full, want)
	}
}

// The statements whose text is FIXED at generate time carry their own ordinals:
// nothing else will number them. Leaving the bare sigil there produced
// `Must declare the scalar variable "@"` from every server that saw one.
func TestFixedStatementsNumberTheirOwnParameters(t *testing.T) {
	live := mssql.Live("")
	for _, c := range []struct{ name, sql string }{
		{"TopNLateral", mssql.TopNLateral("posts", []string{"id"}, "author_id", "UNIQUEIDENTIFIER", live)},
		{"TopNWindow", mssql.TopNWindow("posts", []string{"id"}, "author_id", "UNIQUEIDENTIFIER", live)},
		{"Recursive", mssql.Recursive("nodes", []string{"id"}, "id", "parent_id",
			"UNIQUEIDENTIFIER", mssql.Descend, live)},
	} {
		// A bare sigil is one not followed by the prefix and a digit.
		for i := 0; i < len(c.sql); i++ {
			if c.sql[i] != '@' {
				continue
			}
			rest := c.sql[i+1:]
			if len(rest) < 2 || rest[0] != 'p' || rest[1] < '0' || rest[1] > '9' {
				t.Errorf("%s left an unnumbered parameter at %d:\n  %s", c.name, i, c.sql)
				break
			}
		}
		if !strings.Contains(c.sql, "@p1") || !strings.Contains(c.sql, "@p2") {
			t.Errorf("%s does not bind both the key list and the cap:\n  %s", c.name, c.sql)
		}
	}
}

// The key list is one bound document, so the statement's text does not depend
// on how many keys the caller passed (ADR-0010) — and the unpacked column is
// TYPED, or the comparison is an implicit conversion on the indexed side.
func TestBoundKeyListIsOneParameterAndTyped(t *testing.T) {
	sql := mssql.TopNLateral("posts", []string{"id"}, "author_id", "UNIQUEIDENTIFIER", "")
	if !strings.Contains(sql, "OPENJSON(@p1) WITH ([_storm_k] UNIQUEIDENTIFIER '$')") {
		t.Errorf("the key list is not a typed OPENJSON:\n  %s", sql)
	}
	if strings.Contains(sql, "CROSS JOIN LATERAL") {
		t.Errorf("LATERAL is spelled CROSS APPLY here:\n  %s", sql)
	}
	if !strings.Contains(sql, "TOP (@p2)") {
		t.Errorf("TOP needs a parenthesised parameter:\n  %s", sql)
	}

	a, b := mssql.InFrag(mssql.Ident("id"), "BIGINT", false)
	if !strings.Contains(a+b, "OPENJSON(@) WITH ([v] BIGINT '$')") {
		t.Errorf("an IN list is not a typed OPENJSON: %s%s", a, b)
	}
	if n := strings.Count(a+b, mssql.Placeholder); n != 1 {
		t.Errorf("an IN list binds %d parameters; the whole point is one", n)
	}
	neg, _ := mssql.InFrag(mssql.Ident("id"), "BIGINT", true)
	if !strings.Contains(neg, " NOT IN (") {
		t.Errorf("NotIn did not negate: %s", neg)
	}
}

// A recursive CTE needs no keyword, the guard has no width to overflow, and the
// statement lifts the server's own 100-level ceiling because the real bound is
// the caller's depth parameter.
func TestRecursionGuardsWithoutAWidth(t *testing.T) {
	sql := mssql.Recursive("nodes", []string{"id", "parent_id"}, "id", "parent_id",
		"UNIQUEIDENTIFIER", mssql.Descend, mssql.Live("[deleted_at] IS NULL"))
	if strings.Contains(sql, "WITH RECURSIVE") {
		t.Errorf("T-SQL has no RECURSIVE keyword:\n  %s", sql)
	}
	if !strings.Contains(sql, "AS NVARCHAR(MAX)") {
		t.Errorf("the path has a declared width, so a deep traversal overflows it:\n  %s", sql)
	}
	if !strings.HasSuffix(sql, "OPTION (MAXRECURSION 0)") {
		t.Errorf("OPTION must be the last clause of the statement:\n  %s", sql)
	}
	if mssql.MaxRecursionDepth("UNIQUEIDENTIFIER") != 0 {
		t.Error("an unbounded path claims a depth limit it does not have")
	}
	// BOTH halves carry the soft-delete predicate, or a deleted row rejoins on
	// the second iteration carrying its whole subtree.
	if n := strings.Count(sql, "IS NULL"); n < 2 {
		t.Errorf("the guard is on %d of the two halves:\n  %s", n, sql)
	}
	// Ascend and Descend differ only in which side of the join is the parent.
	up := mssql.Recursive("nodes", []string{"id"}, "id", "parent_id",
		"UNIQUEIDENTIFIER", mssql.Ascend, "")
	down := mssql.Recursive("nodes", []string{"id"}, "id", "parent_id",
		"UNIQUEIDENTIFIER", mssql.Descend, "")
	if up == down {
		t.Error("Ascend and Descend produced the same statement")
	}
}

// NULLS placement does not exist; the two placements storm can ask for are the
// ones this server already has, so both are spelled plainly rather than bought
// with a sort.
func TestOrderTermsBuyNoSort(t *testing.T) {
	for dir, want := range map[int]string{0: "[a]", 1: "[a] DESC", 2: "[a]", 3: "[a] DESC"} {
		if got := mssql.OrderTerm(dir, "[a]"); got != want {
			t.Errorf("OrderTerm(%d) = %s, want %s", dir, got, want)
		}
	}
}

// The operators with no honest lowering are refused BY NAME. A predicate that
// silently disappears returns every row, which is the failure a refusal exists
// to prevent.
func TestRefusedOperatorsSayWhy(t *testing.T) {
	for _, op := range []string{"JSONContains", "JSONContainedBy", "HasAllKeys"} {
		if mssql.Supported(op) {
			t.Errorf("%s reports supported", op)
		}
		if _, _, ok := mssql.Frag(op, "[doc]"); ok {
			t.Errorf("%s produced a fragment", op)
		}
		if why := mssql.Refused(op); why == "" {
			t.Errorf("%s is refused and says nothing about why", op)
		}
	}
	// HasAnyKey IS expressible — one bound array, joined against the document's
	// keys — so it is not in the refused set.
	if !mssql.Supported("HasAnyKey") {
		t.Error("HasAnyKey has a lowering and should report supported")
	}
	a, b, ok := mssql.Frag("HasAnyKey", "[doc]")
	if !ok || !strings.Contains(a+b, "OPENJSON") {
		t.Errorf("HasAnyKey did not lower through OPENJSON: %s%s", a, b)
	}
}

// FILTER has no clause here and the rewrite changes what the aggregate counts,
// so it is refused rather than approximated.
func TestFilterIsRefused(t *testing.T) {
	cond := schema.Cond{Kind: schema.CondIsNotNull,
		Left: schema.Expr{Kind: schema.ExprCol, Col: "paid_at"}}
	e := schema.Expr{Kind: schema.ExprAgg, Fn: "count",
		Args:   []schema.Expr{{Kind: schema.ExprStar}},
		Filter: &cond}
	if _, err := mssql.Expr(e); err == nil {
		t.Fatal("an aggregate FILTER was lowered")
	}
}

// A RANGE frame takes only UNBOUNDED and CURRENT ROW. ROWS accepts the offset,
// but the two differ over ties, so substituting one changes the frame.
func TestNumericRangeFrameIsRefused(t *testing.T) {
	frame := schema.Frame{Kind: schema.FrameRange,
		Start: schema.FrameBound{Kind: schema.Preceding, N: 3},
		End:   schema.FrameBound{Kind: schema.CurrentRow}}
	win := schema.Window{Frame: &frame}
	e := schema.Expr{Kind: schema.ExprWindow, Fn: "sum",
		Args: []schema.Expr{{Kind: schema.ExprCol, Col: "total"}}, Over: &win}
	if _, err := mssql.Expr(e); err == nil {
		t.Fatal("RANGE BETWEEN 3 PRECEDING was lowered")
	}

	// The same frame as ROWS is fine.
	frame.Kind = schema.FrameRows
	if _, err := mssql.Expr(e); err != nil {
		t.Fatalf("ROWS BETWEEN 3 PRECEDING was refused: %v", err)
	}
}

// Integer division truncates here where MySQL's yields a decimal, so the cast
// is load-bearing rather than decoration.
func TestDivisionKeepsItsScale(t *testing.T) {
	e := schema.Expr{Kind: schema.ExprBinary, Arith: schema.ArithDiv,
		Type: schema.Type{Name: schema.TypeNumeric, Precision: 12, Scale: 2},
		Args: []schema.Expr{
			{Kind: schema.ExprCol, Col: "total"},
			{Kind: schema.ExprCol, Col: "count"},
		}}
	got, err := mssql.Expr(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "CAST(") {
		t.Errorf("two INTs divide as integers here; without a cast the fraction is "+
			"gone before ROUND sees it: %s", got)
	}
	if !strings.HasPrefix(got, "ROUND(") {
		t.Errorf("the declared scale is not applied: %s", got)
	}
}

// An unknown arithmetic operator is refused rather than falling through to a
// token that means something else.
func TestUnknownArithmeticIsRefused(t *testing.T) {
	e := schema.Expr{Kind: schema.ExprBinary, Arith: schema.ArithOp("nope"),
		Args: []schema.Expr{
			{Kind: schema.ExprCol, Col: "a"},
			{Kind: schema.ExprCol, Col: "b"},
		}}
	if _, err := mssql.Expr(e); err == nil {
		t.Fatal("an unknown operator was lowered")
	}
}

// A soft-delete predicate must be qualified where it is read under an alias, or
// it is ambiguous when both tables have the column and wrong when one does not.
func TestSoftDeletePredicateQualifies(t *testing.T) {
	if got := mssql.LiveFor("", "deleted_at"); got != "[deleted_at] IS NULL" {
		t.Errorf("unqualified predicate = %s", got)
	}
	if got := mssql.LiveFor("u", "deleted_at"); got != "[u].[deleted_at] IS NULL" {
		t.Errorf("qualified predicate = %s", got)
	}
	if got := mssql.LiveFor("u", ""); got != "" {
		t.Errorf("a table that does not soft-delete got a predicate: %s", got)
	}
	// The semi-join re-qualifies the child's predicate to the exists alias, or
	// "this parent has a related row" is satisfied by a row the child's own
	// package would refuse to return.
	got := mssql.ExistsFrag("posts", "author_id", "authors", "id",
		mssql.Live("[deleted_at] IS NULL"))
	if !strings.Contains(got, "[_storm_e].[deleted_at] IS NULL") {
		t.Errorf("the child predicate was not re-qualified:\n  %s", got)
	}
	if !strings.HasPrefix(mssql.NotExistsFrag("posts", "author_id", "authors", "id", ""), "NOT ") {
		t.Error("NotExistsFrag did not negate")
	}
	// The open forms are the closed ones without their final paren, so the two
	// cannot drift.
	if mssql.ExistsOpen("posts", "author_id", "authors", "id", "")+")" !=
		mssql.ExistsFrag("posts", "author_id", "authors", "id", "") {
		t.Error("ExistsOpen and ExistsFrag disagree")
	}
}

// The clock is the server's, at a resolution two rows stamped in one statement
// can be told apart by. GETDATE() is local, 3.33 ms and the wrong type.
func TestServerClockIsOffsetAwareAndPrecise(t *testing.T) {
	a, _ := mssql.NowFrag("updated_at")
	if !strings.Contains(a, "SYSDATETIMEOFFSET()") {
		t.Errorf("NowFrag = %s", a)
	}
	if !strings.Contains(mssql.SoftDeleteSet("t", "deleted_at"), "SYSDATETIMEOFFSET()") {
		t.Error("the delete mark and the clock disagree about which function to use")
	}
	if !strings.Contains(mssql.RestoreSet("t", "deleted_at"), "= NULL") {
		t.Error("restore does not clear the mark")
	}
}

// Column types come from compile/msddl rather than a second map: two maps for
// one question drift, and the direction is a key the DDL declared one way and
// the loader unpacks another.
func TestColumnTypeDelegatesToTheDDL(t *testing.T) {
	c := &schema.Column{Name: "id", Type: schema.Type{Name: schema.TypeUUID}}
	if got := mssql.ColumnType(c); got != "UNIQUEIDENTIFIER" {
		t.Errorf("ColumnType(uuid) = %s", got)
	}
	// A type msddl refuses cannot reach a generated package — Check runs first
	// — so the fallback exists only to keep the function total.
	bad := &schema.Column{Name: "x", Type: schema.Type{Name: schema.TypeInet}}
	if got := mssql.ColumnType(bad); got == "" {
		t.Error("ColumnType is not total")
	}
}
