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

// An INSERT … SELECT names the assigned columns and then the reference, and
// binds only the assigned ones: the reference comes from the SELECT.
func TestSpliceInsertSelectBindsOnlyTheConstants(t *testing.T) {
	p := runtime.InsertParts{Open: " (", Sep: ", ", Mid: ") VALUES (", Close: ")"}
	st := runtime.SpliceInsertSelect(`INSERT INTO "items"`, p, []string{`"batch_id"`, `"status"`}, `"instance_id"`, ") SELECT ", runtime.Placeholder{})
	want := `INSERT INTO "items" ("batch_id", "status", "instance_id") SELECT $1, $2, `
	if st.SQL != want || st.NArg != 2 {
		t.Errorf("got %q (%d args), want %q (2)", st.SQL, st.NArg, want)
	}
	st = runtime.SpliceInsertSelect(`INSERT INTO "items"`, p, nil, `"instance_id"`, ") SELECT ", runtime.Placeholder{})
	if want := `INSERT INTO "items" ("instance_id") SELECT `; st.SQL != want || st.NArg != 0 {
		t.Errorf("with nothing assigned got %q (%d args), want %q", st.SQL, st.NArg, want)
	}
}

// A write after a step numbers its placeholders on from the step's, and its
// count says so, so a predicate spliced after it carries on from there.
func TestSpliceSectionsFromNumbersOnFromTheStep(t *testing.T) {
	st := runtime.SpliceSectionsFrom(`WITH s AS (…$1, $2) UPDATE "t"`, []runtime.Section{
		{Lead: " SET ", Sep: ", ", Frags: []runtime.Frag{{A: `"a" = $`}, {A: `"b" = now()`}, {A: `"c" = CASE WHEN x THEN "c" ELSE $`, B: " END"}}},
	}, "", 2)
	want := `WITH s AS (…$1, $2) UPDATE "t" SET "a" = $3, "b" = now(), "c" = CASE WHEN x THEN "c" ELSE $4 END`
	if st.SQL != want || st.NArg != 4 {
		t.Errorf("got %q (%d args), want %q (4)", st.SQL, st.NArg, want)
	}
}

func TestHashStringTellsTextsApart(t *testing.T) {
	a, b := runtime.HashString(`UPDATE "a"`), runtime.HashString(`UPDATE "b"`)
	if a == b || a != runtime.HashString(`UPDATE "a"`) {
		t.Errorf("HashString is not a stable, discriminating key: %x %x", a, b)
	}
}
