package runtime

import "strings"

// MergeParts carries the punctuation of a MERGE from the back end to the
// splicer, the way InsertParts does for an INSERT.
//
// A back end whose upsert is a CLAUSE on the insert — PostgreSQL's ON CONFLICT
// — needs none of this: the insert is spliced as usual and the clause is a
// suffix. SQL Server's is a different STATEMENT, so the whole shape has to come
// from the back end, and the splicer does what it always does: concatenation,
// and no keyword of its own.
//
// Every field is punctuation. The identifiers and the placeholders are the
// splicer's; the words are compile/mssql's.
type MergeParts struct {
	// Into opens the statement and the source row:
	// `MERGE [t] WITH (HOLDLOCK) AS [_storm_m] USING (VALUES (`
	Into string
	// Sep separates values, columns and assignments alike.
	Sep string
	// AsSrc closes the source row and opens its column list:
	// `)) AS [_storm_s] (`
	AsSrc string
	// OnLead closes the column list and opens the match condition: `) ON `
	OnLead string
	// OnSep joins the key comparisons: ` AND `
	OnSep string
	// OnClose ends the match condition, for a back end that PARENTHESISES it.
	//
	// Empty on SQL Server, `)` on Oracle, where an unparenthesised ON is
	// ORA-00969 "missing ON keyword". It is a field of its own rather than a
	// prefix on Matched and NotMatched because those are alternatives: the
	// splicer writes at most one of the two branches' leading text before the
	// other, and a paren in both would leave a stray one in the form that has
	// a MATCHED branch.
	OnClose string
	// Eq is the comparison and the assignment operator alike: ` = `
	Eq string
	// Tgt and Src qualify a column to the target or the source row.
	Tgt, Src string
	// Matched opens the update branch: ` WHEN MATCHED THEN UPDATE SET `
	Matched string
	// NotMatched opens the insert branch:
	// ` WHEN NOT MATCHED THEN INSERT (`
	NotMatched string
	// Values closes the insert's column list and opens its value list.
	Values string
	// Close closes a parenthesised list.
	Close string
	// End terminates the statement, which MERGE requires and nothing else does.
	End string
}

// SpliceMerge assembles an upsert.
//
// cols are the columns being inserted, already quoted; keys are the columns the
// match is ON; set are the columns to overwrite when a row already exists, and
// an empty set means leave it alone — a MERGE with only a NOT MATCHED branch,
// which is the idempotent insert.
//
// The parameters are numbered once, in the source row, and referred to by name
// after that. That is what makes this expressible at all: the insert branch
// and the update branch both read the SAME row, and a positional back end would
// have to bind every value twice.
func SpliceMerge(p MergeParts, cols, keys, set []string, ph Placeholder, out string) *Stmt {
	var b strings.Builder
	b.Grow(len(p.Into) + len(cols)*24 + len(set)*24 + len(out) + 64)

	b.WriteString(p.Into)
	for i := range cols {
		if i > 0 {
			b.WriteString(p.Sep)
		}
		ph.write(&b, i+1)
	}
	b.WriteString(p.AsSrc)
	for i, c := range cols {
		if i > 0 {
			b.WriteString(p.Sep)
		}
		b.WriteString(c)
	}

	b.WriteString(p.OnLead)
	for i, k := range keys {
		if i > 0 {
			b.WriteString(p.OnSep)
		}
		b.WriteString(p.Tgt)
		b.WriteString(k)
		b.WriteString(p.Eq)
		b.WriteString(p.Src)
		b.WriteString(k)
	}
	b.WriteString(p.OnClose)

	if len(set) > 0 {
		b.WriteString(p.Matched)
		for i, c := range set {
			if i > 0 {
				b.WriteString(p.Sep)
			}
			b.WriteString(c)
			b.WriteString(p.Eq)
			b.WriteString(p.Src)
			b.WriteString(c)
		}
	}

	b.WriteString(p.NotMatched)
	for i, c := range cols {
		if i > 0 {
			b.WriteString(p.Sep)
		}
		b.WriteString(c)
	}
	b.WriteString(p.Values)
	for i, c := range cols {
		if i > 0 {
			b.WriteString(p.Sep)
		}
		b.WriteString(p.Src)
		b.WriteString(c)
	}
	b.WriteString(p.Close)

	b.WriteString(out)
	b.WriteString(p.End)
	return &Stmt{SQL: b.String(), NArg: len(cols)}
}
