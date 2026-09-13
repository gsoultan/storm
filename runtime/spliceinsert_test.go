package runtime_test

// The insert splicer, which is the cold path a partial or queued insert takes.
//
// A full-row insert uses SQL fixed at generate time and so never comes through
// here. Every OTHER insert does — Create(), Ins, InsertOp, the whole unit of
// work — and it appended an ordinal to the placeholder unconditionally. That is
// PostgreSQL's `$1` with the sigil swapped, which on a bare back end is `?1`
// and MySQL rejects it.

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

var insParts = runtime.InsertParts{Open: " (", Sep: ", ", Mid: ") VALUES (", Close: ")"}

func TestSpliceInsertNumbersPostgresPlaceholders(t *testing.T) {
	st := runtime.SpliceInsertWith("INSERT INTO \"t\"", insParts,
		[]string{`"a"`, `"b"`}, runtime.Placeholder{}, ` RETURNING "a"`)
	want := `INSERT INTO "t" ("a", "b") VALUES ($1, $2) RETURNING "a"`
	if st.SQL != want {
		t.Errorf("SQL = %s\nwant %s", st.SQL, want)
	}
	if st.NArg != 2 {
		t.Errorf("NArg = %d, want 2", st.NArg)
	}
}

// The bare form, and the reason this function grew a carrier.
func TestSpliceInsertLeavesABarePlaceholderBare(t *testing.T) {
	st := runtime.SpliceInsertWith("INSERT INTO `t`", insParts,
		[]string{"`a`", "`b`", "`c`"}, runtime.MySQLPlaceholder, "")
	want := "INSERT INTO `t` (`a`, `b`, `c`) VALUES (?, ?, ?)"
	if st.SQL != want {
		t.Errorf("SQL = %s\nwant %s", st.SQL, want)
	}
	// The specific wrong output, named so a regression is unmistakable.
	if strings.Contains(st.SQL, "?1") {
		t.Error("an ordinal was appended to a bare placeholder — MySQL rejects `?1`")
	}
	if st.NArg != 3 {
		t.Errorf("NArg = %d, want 3", st.NArg)
	}
}

// The string form stays, because it is what a generated package emitted before
// the carrier existed and a regenerated package is not a required upgrade.
func TestSpliceInsertStringFormStillNumbers(t *testing.T) {
	st := runtime.SpliceInsert("INSERT INTO \"t\"", insParts, []string{`"a"`}, "$", "")
	if st.SQL != `INSERT INTO "t" ("a") VALUES ($1)` {
		t.Errorf("SQL = %s", st.SQL)
	}
}

// One column and none: the separator must not appear at either edge.
func TestSpliceInsertEdges(t *testing.T) {
	one := runtime.SpliceInsertWith("I", insParts, []string{"a"}, runtime.MySQLPlaceholder, "")
	if one.SQL != "I (a) VALUES (?)" {
		t.Errorf("one column = %q", one.SQL)
	}
	none := runtime.SpliceInsertWith("I", insParts, nil, runtime.MySQLPlaceholder, "")
	if none.SQL != "I () VALUES ()" {
		t.Errorf("no columns = %q", none.SQL)
	}
	if none.NArg != 0 {
		t.Errorf("NArg = %d for no columns", none.NArg)
	}
}
