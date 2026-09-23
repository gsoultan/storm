package mssql

// The upsert, which is a STATEMENT here rather than a clause.
//
// PostgreSQL's `INSERT … ON CONFLICT (email) DO UPDATE SET …` is one insert
// with a tail. SQL Server's is MERGE: a target, a source, a match condition and
// a branch per outcome. Nothing about it can be appended to an insert, which is
// why the seam takes the whole shape from here rather than a suffix.
//
// TWO THINGS ABOUT MERGE ARE NOT OPTIONAL, and both are in Parts below.
//
// WITH (HOLDLOCK). Without it MERGE is NOT atomic against a concurrent MERGE of
// the same key: both read, both find no row, both take the NOT MATCHED branch,
// and one gets a primary key violation. The hint takes the range lock that
// makes the read-and-write a single decision. It is the single most
// misunderstood thing about this statement, it is a correctness matter rather
// than a tuning one, and a generator that left it out would produce an upsert
// that works in every test and fails under load.
//
// The SEMICOLON. MERGE is the one statement T-SQL requires to be terminated,
// and the error for omitting it names the next statement rather than this one.
//
// What is NOT generated here, and why: the UNTARGETED `DoNothing()`.
// PostgreSQL's bare `ON CONFLICT DO NOTHING` fires on ANY unique index, and
// MERGE's ON clause names specific columns — there is no way to say "whichever
// index it was". A method that silently watched the primary key instead would
// be a lie at the call site, so codegen does not emit it and calling it is an
// undefined-method compile error naming exactly what is missing. The TARGETED
// form, `OnConflictEmail().DoNothing()`, is a MERGE with no MATCHED branch and
// is generated.

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
		// HOLDLOCK on the TARGET, which is what makes the match and the write
		// one decision. See the note at the top of this file.
		Into:       "MERGE " + Ident(table) + " WITH (HOLDLOCK) AS " + Ident(mergeTarget) + " USING (VALUES (",
		Sep:        ", ",
		AsSrc:      ")) AS " + Ident(mergeSource) + " (",
		OnLead:     ") ON ",
		OnSep:      " AND ",
		Eq:         " = ",
		Tgt:        Ident(mergeTarget) + ".",
		Src:        Ident(mergeSource) + ".",
		Matched:    " WHEN MATCHED THEN UPDATE SET ",
		NotMatched: " WHEN NOT MATCHED THEN INSERT (",
		Values:     ") VALUES (",
		Close:      ")",
		End:        ";",
	}
}

// MergeOutput is the clause a MERGE hands its row back through.
//
// INSERTED for both branches: on the insert branch it is the row that was
// written, and on the update branch it is the row AFTER the update — which is
// what an upsert's caller wants in either case, and the reason a caller can
// read the server-generated key back from an upsert at all.
//
// It goes after the WHEN clauses and before the terminator, which is a third
// position for a clause that is a suffix everywhere else.
func MergeOutput(cols []string) string { return ReturningClause(cols) }
