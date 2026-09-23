package oracle

// The upsert, which is a STATEMENT here rather than a clause — as it is on
// SQL Server and for the same reason: Oracle has MERGE and no ON CONFLICT.
//
// FOUR THINGS ABOUT IT WERE MEASURED RATHER THAN READ, and two came back the
// opposite way round from what the documentation suggested
// (internal/oraclespike, TestWhatAnOracleUpsertWouldCost):
//
//  1. The source CAN be a row constructor. `USING (VALUES (:1, :2)) s (a, b)`
//     works on 23 — so runtime.MergeParts' existing shape fits, and the
//     `SELECT … FROM dual` form every Oracle example uses is not required.
//     That shape was the reason this looked unsupportable.
//  2. The ON condition REQUIRES parentheses. An unparenthesised one is
//     ORA-00969, "missing ON keyword", and MergeParts had no slot for the
//     closing bracket — which is why it grew OnClose.
//  3. A MERGE with no MATCHED branch is legal, so the idempotent insert —
//     `OnConflictEmail().DoNothing()` — needs nothing special.
//  4. MERGE CAN RETURN. `RETURNING "m"."id" INTO :1` works and handed the row
//     back. storm still does not use it, for the reason InsertStmt does not:
//     the INTO binds an OUTPUT parameter and runtime.Executor carries none.
//     So the upsert writes and reports nothing, which is what canReturn()
//     already says about this target.
//
// AND ONE THING THAT IS NOT HERE. SQL Server's Into carries WITH (HOLDLOCK),
// without which two concurrent merges of one key both take the NOT MATCHED
// branch and one gets a primary key violation. Oracle needs no hint for that
// and has none to give: its MERGE takes the row locks it needs as it matches.
// What it does NOT promise is that a concurrent INSERT of the same key between
// the match and the write cannot happen — that surfaces as ORA-00001 on the
// unique index, which is an error the caller sees rather than a wrong row.

const (
	mergeTarget = "_storm_m"
	mergeSource = "_storm_s"
)

// MergeParts is the punctuation a MERGE is spliced from. The field names and
// the order mirror runtime.MergeParts, which is what receives it.
type MergeParts struct {
	Into, Sep, AsSrc, OnLead, OnSep, OnClose, Eq, Tgt, Src string
	Matched, NotMatched, Values, Close, End                string
}

// Merge returns the punctuation for one table.
func Merge(table string) MergeParts {
	return MergeParts{
		// No AS before the alias: a table alias is juxtaposition here and
		// `MERGE INTO t AS m` is ORA-00933.
		Into:    "MERGE INTO " + Ident(table) + " " + Ident(mergeTarget) + " USING (VALUES (",
		Sep:     ", ",
		AsSrc:   ")) " + Ident(mergeSource) + " (",
		OnLead:  ") ON (",
		OnSep:   " AND ",
		OnClose: ")", // (2) above: ORA-00969 without it
		Eq:      " = ",
		Tgt:     Ident(mergeTarget) + ".",
		Src:     Ident(mergeSource) + ".",
		Matched: " WHEN MATCHED THEN UPDATE SET ",
		// The target must be QUALIFIED in the SET list here, unlike SQL
		// Server's, where a bare column name is resolved to the target.
		NotMatched: " WHEN NOT MATCHED THEN INSERT (",
		Values:     ") VALUES (",
		Close:      ")",
		// No terminator. MERGE is the one statement T-SQL requires one for;
		// through Oracle's protocol a trailing `;` is ORA-00911.
		End: "",
	}
}

// MergeOutput is empty for every column list.
//
// Oracle's MERGE CAN return — measured, and it handed the row back — and storm
// cannot use it: the `INTO` binds an OUTPUT parameter that runtime.Executor
// has nowhere to put. The same reason InsertStmt refuses a returning list, and
// the same reason keys are generated client-side.
func MergeOutput([]string) string { return "" }
