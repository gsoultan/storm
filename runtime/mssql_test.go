package runtime_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

// The three things a named-parameter back end needs from the splicer that a
// positional one does not. Each is a construct SQL Server has no other spelling
// for, so each is a generation error or a syntax error without it.

// A prefix makes the ordinal part of a legal parameter NAME. `@1` is not a
// terse parameter; T-SQL identifiers may not begin with a digit, so it is a
// syntax error, and every statement storm generates would carry one.
func TestPlaceholderPrefixNamesTheParameter(t *testing.T) {
	lw := runtime.Lowering{
		Frag:        func(op, col uint32) runtime.Frag { return runtime.Frag{A: "[c] = @"} },
		Order:       func(dir, col uint32) string { return "[c]" },
		OB:          runtime.Order{Lead: " ORDER BY ", Sep: ", "},
		Placeholder: runtime.MSSQLPlaceholder,
	}
	toks := []runtime.Tok{runtime.MakeLeaf(0, 0)}

	st := runtime.SpliceTree("SELECT 1 FROM [t]", toks, lw,
		" OFFSET 0 ROWS FETCH NEXT @ ROWS ONLY")
	want := "SELECT 1 FROM [t] WHERE [c] = @p1 OFFSET 0 ROWS FETCH NEXT @p2 ROWS ONLY"
	if st.SQL != want {
		t.Errorf("got  %s\nwant %s", st.SQL, want)
	}
	if st.NArg != 2 {
		t.Errorf("NArg = %d, want 2", st.NArg)
	}
}

// The guard against re-numbering has to see the PREFIX, not a digit. Scanning
// `@p1` for a digit one past the sigil finds `p`, numbers the sigil anyway and
// emits `@1p1` — two parameter names where the generator wrote one.
func TestSpliceLeavesAlreadyNamedParametersAlone(t *testing.T) {
	lw := runtime.Lowering{
		Frag:        func(op, col uint32) runtime.Frag { return runtime.Frag{A: "[c] = @"} },
		Order:       func(dir, col uint32) string { return "[c]" },
		OB:          runtime.Order{Lead: " ORDER BY ", Sep: ", "},
		Placeholder: runtime.MSSQLPlaceholder,
	}
	toks := []runtime.Tok{runtime.MakeLeaf(0, 0)}

	st := runtime.SpliceTree("SELECT 1 FROM [t]", toks, lw,
		" HAVING x > @p1 OFFSET 0 ROWS FETCH NEXT @ ROWS ONLY")
	want := "SELECT 1 FROM [t] WHERE [c] = @p1 HAVING x > @p1 " +
		"OFFSET 0 ROWS FETCH NEXT @p2 ROWS ONLY"
	if st.SQL != want {
		t.Errorf("got  %s\nwant %s", st.SQL, want)
	}
	if strings.Contains(st.SQL, "@1") {
		t.Errorf("a prefixed name was numbered as though it were bare: %s", st.SQL)
	}
}

// Keyset pagination is not optional — it is how storm pages without OFFSET
// walking rows it already showed — and SQL Server has no row constructor, so
// the comparison has to be written as the OR-chain it means.
func TestRowComparisonExpandsWhereThereIsNoRowConstructor(t *testing.T) {
	cols := []string{"[a]", "[b]", "[c]"}
	lw := runtime.Lowering{
		Frag:         func(op, col uint32) runtime.Frag { return runtime.Frag{} },
		Order:        func(dir, col uint32) string { return "[a]" },
		OB:           runtime.Order{Lead: " ORDER BY ", Sep: ", "},
		Ident:        func(col uint32) string { return cols[col] },
		RowCmp:       func(op uint32) string { return " > " },
		RowCmpExpand: true,
		Placeholder:  runtime.MSSQLPlaceholder,
	}
	toks := []runtime.Tok{
		runtime.MakeCol(0), runtime.MakeCol(1), runtime.MakeCol(2),
		runtime.MakeRowCmp(0, 3),
	}

	st := runtime.SpliceTree("SELECT 1 FROM [t]", toks, lw, "")
	// The outermost parens are the splicer's to strip when this is the whole
	// predicate — `WHERE (X)` is `WHERE X` — and it keeps them when the
	// expansion is one branch of a larger tree, which is where they matter.
	want := "SELECT 1 FROM [t] WHERE [a] > @p1 OR ([a] = @p1 AND " +
		"([b] > @p2 OR ([b] = @p2 AND [c] > @p3)))"
	if st.SQL != want {
		t.Errorf("got  %s\nwant %s", st.SQL, want)
	}

	// Three columns, three VALUES — the binder appends one per column, and a
	// parameter mentioned twice is still one parameter because it is named.
	if st.NArg != 3 {
		t.Errorf("NArg = %d, want 3 — the expansion mentions each name twice "+
			"but binds each value once", st.NArg)
	}
}

// The tuple form is untouched for the back ends that have it, which is what
// keeps PostgreSQL's emitted text byte-identical.
func TestRowComparisonKeepsTheTupleFormWhereThereIsOne(t *testing.T) {
	cols := []string{`"a"`, `"b"`}
	lw := runtime.Lowering{
		Frag:       func(op, col uint32) runtime.Frag { return runtime.Frag{} },
		Order:      func(dir, col uint32) string { return `"a"` },
		OB:         runtime.Order{Lead: " ORDER BY ", Sep: ", "},
		Ident:      func(col uint32) string { return cols[col] },
		RowCmp:     func(op uint32) string { return " > " },
		TupleOpen:  "(",
		TupleSep:   ", ",
		TupleClose: ")",
	}
	toks := []runtime.Tok{runtime.MakeCol(0), runtime.MakeCol(1), runtime.MakeRowCmp(0, 2)}

	st := runtime.SpliceTree("SELECT 1 FROM t", toks, lw, "")
	if want := `SELECT 1 FROM t WHERE ("a", "b") > ($1, $2)`; st.SQL != want {
		t.Errorf("got  %s\nwant %s", st.SQL, want)
	}
}

// OFFSET/FETCH is defined as part of ORDER BY, so a capped read with no
// ordering is a syntax error rather than an unordered result.
func TestOrderFallbackOnlyWhereThereIsSomethingToOrderFor(t *testing.T) {
	lw := runtime.Lowering{
		Frag:          func(op, col uint32) runtime.Frag { return runtime.Frag{A: "[c] = @"} },
		Order:         func(dir, col uint32) string { return "[c]" },
		OB:            runtime.Order{Lead: " ORDER BY ", Sep: ", "},
		Placeholder:   runtime.MSSQLPlaceholder,
		OrderFallback: " ORDER BY (SELECT NULL)",
	}
	toks := []runtime.Tok{runtime.MakeLeaf(0, 0)}

	// A capped read with no ordering tokens gets the fallback.
	st := runtime.SpliceTree("SELECT [a] FROM [t]", toks, lw,
		" OFFSET 0 ROWS FETCH NEXT @ ROWS ONLY")
	want := "SELECT [a] FROM [t] WHERE [c] = @p1 ORDER BY (SELECT NULL) " +
		"OFFSET 0 ROWS FETCH NEXT @p2 ROWS ONLY"
	if st.SQL != want {
		t.Errorf("got  %s\nwant %s", st.SQL, want)
	}

	// A count carries no suffix and must not gain an ORDER BY — ordering a
	// scalar is a sort for nothing, and inside a derived table it is illegal.
	st = runtime.SpliceTree("SELECT count(*) FROM [t]", toks, lw, "")
	if want := "SELECT count(*) FROM [t] WHERE [c] = @p1"; st.SQL != want {
		t.Errorf("got  %s\nwant %s", st.SQL, want)
	}

	// An ordering the caller asked for suppresses it.
	ordered := append([]runtime.Tok{}, toks...)
	ordered = append(ordered, runtime.MakeOrder(0, 0))
	st = runtime.SpliceTree("SELECT [a] FROM [t]", ordered, lw,
		" OFFSET 0 ROWS FETCH NEXT @ ROWS ONLY")
	if strings.Count(st.SQL, "ORDER BY") != 1 {
		t.Errorf("two orderings in one statement: %s", st.SQL)
	}
}

// SQL Server's OUTPUT sits between the assignments and the predicate. At the
// end — where RETURNING goes on both other targets — it is a syntax error.
func TestOutputClauseLandsBeforeThePredicate(t *testing.T) {
	set := runtime.Section{
		Lead: " SET ", Sep: ", ",
		Frags: []runtime.Frag{{A: "[a] = @"}, {A: "[b] = @"}},
	}
	where := runtime.Section{
		Lead: " WHERE ", Sep: " AND ",
		Frags: []runtime.Frag{{A: "[id] = @"}},
	}
	const out = " OUTPUT INSERTED.[a], INSERTED.[b]"

	st := runtime.SpliceSectionsOutput("UPDATE [t]", []runtime.Section{set, where},
		out, "", runtime.MSSQLPlaceholder)
	want := "UPDATE [t] SET [a] = @p1, [b] = @p2" + out + " WHERE [id] = @p3"
	if st.SQL != want {
		t.Errorf("got  %s\nwant %s", st.SQL, want)
	}
	if st.NArg != 3 {
		t.Errorf("NArg = %d, want 3", st.NArg)
	}

	// A delete has no assignments, so its only section is the predicate and the
	// clause lands right after the prefix.
	st = runtime.SpliceSectionsOutput("DELETE FROM [t]", []runtime.Section{where},
		" OUTPUT DELETED.[a]", "", runtime.MSSQLPlaceholder)
	if want := "DELETE FROM [t] OUTPUT DELETED.[a] WHERE [id] = @p1"; st.SQL != want {
		t.Errorf("got  %s\nwant %s", st.SQL, want)
	}

	// An empty predicate leaves the clause at the end, which is where a
	// statement with nothing to precede wants it.
	st = runtime.SpliceSectionsOutput("UPDATE [t]",
		[]runtime.Section{set, {Lead: " WHERE ", Sep: " AND "}},
		out, "", runtime.MSSQLPlaceholder)
	if want := "UPDATE [t] SET [a] = @p1, [b] = @p2" + out; st.SQL != want {
		t.Errorf("got  %s\nwant %s", st.SQL, want)
	}
}

// The existing splice is the same function with no clause, so the two cannot
// drift apart.
func TestSectionsWithoutAnOutputClauseAreUnchanged(t *testing.T) {
	set := runtime.Section{Lead: " SET ", Sep: ", ", Frags: []runtime.Frag{{A: `"a" = $`}}}
	where := runtime.Section{Lead: " WHERE ", Sep: " AND ", Frags: []runtime.Frag{{A: `"id" = $`}}}
	secs := []runtime.Section{set, where}

	plain := runtime.SpliceSections(`UPDATE "t"`, secs, ` RETURNING "a"`)
	out := runtime.SpliceSectionsOutput(`UPDATE "t"`, secs, "", ` RETURNING "a"`,
		runtime.Placeholder{})
	if plain.SQL != out.SQL || plain.NArg != out.NArg {
		t.Errorf("the two splices disagree:\n  %s\n  %s", plain.SQL, out.SQL)
	}
}
