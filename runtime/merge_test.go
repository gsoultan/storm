package runtime_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

func mergeParts() runtime.MergeParts {
	return runtime.MergeParts{
		Into:       "MERGE [users] WITH (HOLDLOCK) AS [_m] USING (VALUES (",
		Sep:        ", ",
		AsSrc:      ")) AS [_s] (",
		OnLead:     ") ON ",
		OnSep:      " AND ",
		Eq:         " = ",
		Tgt:        "[_m].",
		Src:        "[_s].",
		Matched:    " WHEN MATCHED THEN UPDATE SET ",
		NotMatched: " WHEN NOT MATCHED THEN INSERT (",
		Values:     ") VALUES (",
		Close:      ")",
		End:        ";",
	}
}

// The upsert, for a back end whose conflict handling is a STATEMENT rather than
// a clause. Every parameter is numbered ONCE, in the source row, and referred
// to by name after that — which is what makes both branches able to read the
// same row without binding every value twice.
func TestSpliceMerge(t *testing.T) {
	st := runtime.SpliceMerge(mergeParts(),
		[]string{"[id]", "[email]", "[name]"},
		[]string{"[email]"},
		[]string{"[name]"},
		runtime.MSSQLPlaceholder,
		" OUTPUT INSERTED.[id]")

	want := "MERGE [users] WITH (HOLDLOCK) AS [_m] USING (VALUES (@p1, @p2, @p3)) " +
		"AS [_s] ([id], [email], [name]) ON [_m].[email] = [_s].[email]" +
		" WHEN MATCHED THEN UPDATE SET [name] = [_s].[name]" +
		" WHEN NOT MATCHED THEN INSERT ([id], [email], [name]) " +
		"VALUES ([_s].[id], [_s].[email], [_s].[name])" +
		" OUTPUT INSERTED.[id];"
	if st.SQL != want {
		t.Errorf("got  %s\nwant %s", st.SQL, want)
	}
	// Three columns, three VALUES: a name mentioned in both branches is still
	// one parameter.
	if st.NArg != 3 {
		t.Errorf("NArg = %d, want 3", st.NArg)
	}
}

// An empty SET list is the idempotent insert: no MATCHED branch at all, so the
// row that is already there is left exactly as it is. `UPDATE SET` with no
// assignments is not SQL, so this is a shape rather than an empty string.
func TestSpliceMergeWithNothingToUpdate(t *testing.T) {
	st := runtime.SpliceMerge(mergeParts(),
		[]string{"[id]", "[email]"}, []string{"[email]"}, nil,
		runtime.MSSQLPlaceholder, "")
	if strings.Contains(st.SQL, "WHEN MATCHED") {
		t.Errorf("an empty set list still emitted an update branch:\n%s", st.SQL)
	}
	if !strings.Contains(st.SQL, "WHEN NOT MATCHED THEN INSERT") {
		t.Errorf("no insert branch:\n%s", st.SQL)
	}
	// MERGE is the one statement T-SQL requires to be terminated, and the error
	// for omitting it names the NEXT statement.
	if !strings.HasSuffix(st.SQL, ";") {
		t.Errorf("not terminated:\n%s", st.SQL)
	}
}

// A composite key joins its comparisons rather than matching on the first.
func TestSpliceMergeCompositeKey(t *testing.T) {
	st := runtime.SpliceMerge(mergeParts(),
		[]string{"[org]", "[email]", "[name]"},
		[]string{"[org]", "[email]"},
		[]string{"[name]"},
		runtime.MSSQLPlaceholder, "")
	want := "ON [_m].[org] = [_s].[org] AND [_m].[email] = [_s].[email]"
	if !strings.Contains(st.SQL, want) {
		t.Errorf("the match condition is not the whole key:\n%s", st.SQL)
	}
}

// The HOLDLOCK hint is the back end's, and it is a CORRECTNESS matter rather
// than a tuning one: without it two concurrent merges of the same key both find
// no row, both take the insert branch, and one gets a primary key violation.
// The splicer cannot add it — it writes no keywords — so this asserts it
// survives the splice rather than that it is invented here.
func TestSpliceMergeKeepsTheBackEndsLockHint(t *testing.T) {
	st := runtime.SpliceMerge(mergeParts(), []string{"[id]"}, []string{"[id]"}, nil,
		runtime.MSSQLPlaceholder, "")
	if !strings.Contains(st.SQL, "WITH (HOLDLOCK)") {
		t.Errorf("the lock hint was dropped:\n%s", st.SQL)
	}
}
