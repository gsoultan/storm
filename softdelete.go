package storm

import (
	"fmt"

	"github.com/gsoultan/storm/schema"
)

// Soft-delete validation — the second hazard from docs/CONCEPT.md's rejection
// of soft delete BY DEFAULT: "unique indexes stop meaning what they say".
//
// A marked row is still a row. It keeps its primary key, and it keeps its place
// in every unique key, so a table that soft-deletes a user and then declares
// UNIQUE (email) has quietly promised that the address can never be used again
// — not by a new signup, and not by the same person coming back. Nothing fails
// at declaration time. Nothing fails at migration time. It fails the first time
// somebody re-registers, in production, months later.
//
// PostgreSQL cannot express "unique among the rows that are alive" as a
// CONSTRAINT; only a partial unique INDEX can carry a predicate. So the
// declaration storm has to refuse is exactly the one it cannot make mean what
// the reader thinks it means, and the fix is a different declaration rather
// than a flag.
//
// A partial unique index is NOT inferred silently in its place. That would be
// storm changing the meaning of what the model said, which is the implicitness
// the concept doc rejects GORM's soft delete for in the first place — and
// "unique across every row, deleted or not" is a real thing to want: an audit
// table, an external identifier that must never be reissued, a slug reserved
// permanently once used.
func (b *builder) validateSoftDelete() {
	for _, mi := range b.ordered {
		t := mi.tbl.out
		if !t.SoftDeletes() {
			continue
		}
		if c := t.Column(t.SoftDelete); c != nil && c.NotNull {
			b.errs.add(fmt.Errorf(
				"%s: the soft-delete column %s is NOT NULL, so no row can be alive",
				t.Name, t.SoftDelete))
		}
		for _, u := range t.Uniques {
			b.errs.add(fmt.Errorf(
				"%s: UNIQUE (%s) on a soft-delete table would outlive the rows it constrains — "+
					"a deleted row keeps its key, so the value can never be used again.\n"+
					"       PostgreSQL can only say \"unique among live rows\" as a PARTIAL INDEX:\n"+
					"           t.Index(%s).Unique().Where(\"%s IS NULL\")\n"+
					"       If you meant unique across deleted rows too — an identifier that must "+
					"never be reissued — say so with that same partial form and no Where.",
				t.Name, joinNames(u.Columns), fieldList(u.Columns), t.SoftDelete))
		}
	}
}

// validateSoftDeleteReach refuses the reads storm cannot yet guard.
//
// The design here is "every read path carries the predicate, or the model does
// not build". The base reads — select, count, exists, projections — and every
// write are guarded. The declared cross-table reads are NOT, because their SQL
// names several tables under aliases and the predicate has to be attached to
// the right one; getting that wrong is indistinguishable, from the call site,
// from getting it right.
//
// So they are refused rather than shipped unguarded. A refusal is a compile
// error with a name and a fix in it. The alternative is a join that silently
// returns rows the application has been told are deleted, which is precisely
// the failure docs/CONCEPT.md rejects soft-delete-by-default for — and shipping
// it *inside* the feature meant to avoid it would be the worse joke.
func (b *builder) validateSoftDeleteReach() {
	soft := map[string]bool{}
	for _, mi := range b.ordered {
		if t := mi.tbl.out; t.SoftDeletes() {
			soft[t.Name] = true
		}
	}
	if len(soft) == 0 {
		return
	}
	// rel maps table -> field -> target table, so a plan's field name can be
	// resolved to the table it will actually read.
	rel := map[string]map[string]string{}
	for _, mi := range b.ordered {
		t := mi.tbl.out
		m := map[string]string{}
		for _, r := range t.Relations {
			m[r.Field] = r.Target
		}
		rel[t.Name] = m
	}

	refuse := func(where, what, target string) {
		b.errs.add(fmt.Errorf(
			"%s: %s reads %s, which soft-deletes, and storm does not yet put the "+
				"IS NULL predicate into a declared cross-table read — it would return rows "+
				"the rest of your code has been told are deleted.\n"+
				"       Until it does, read %s through its own generated package (where the "+
				"predicate IS compiled in), or write the query with storm.SQL and say "+
				"\"%s IS NULL\" yourself.",
			where, what, target, target, softCol(b, target)))
	}

	for _, mi := range b.ordered {
		t := mi.tbl.out
		for _, a := range t.Aggregates {
			if soft[t.Name] {
				refuse(t.Name, "aggregate "+a.Name, t.Name)
			}
		}
		for _, j := range t.Joins {
			if soft[t.Name] {
				refuse(t.Name, "join "+j.Name, t.Name)
			}
			for _, jt := range j.Tables {
				if jt.Table != "" && soft[jt.Table] {
					refuse(t.Name, "join "+j.Name, jt.Table)
				}
			}
			for _, c := range j.CTEs {
				if soft[c.Table] {
					refuse(t.Name, "join "+j.Name+" (CTE "+c.Alias+")", c.Table)
				}
			}
		}
		for _, p := range t.Plans {
			var walk func(fs []schema.PlanField, from string)
			walk = func(fs []schema.PlanField, from string) {
				for _, f := range fs {
					tgt := rel[from][f.Field]
					if tgt == "" {
						continue
					}
					if soft[tgt] {
						refuse(t.Name, "plan "+p.Name+" (."+f.Field+")", tgt)
					}
					walk(f.Nested, tgt)
				}
			}
			walk(p.Fields, t.Name)
		}
	}
	for _, u := range b.outSch.Unions {
		for _, br := range u.Branches {
			if soft[br.Table] {
				refuse("union "+u.Name, "branch on "+br.Table, br.Table)
			}
		}
	}
}

// softCol finds a table's soft-delete column for a message.
func softCol(b *builder, table string) string {
	for _, mi := range b.ordered {
		if mi.tbl.out.Name == table {
			return mi.tbl.out.SoftDelete
		}
	}
	return "deleted_at"
}

// joinNames renders a column list for a message.
func joinNames(cols []string) string {
	out := ""
	for i, c := range cols {
		if i > 0 {
			out += ", "
		}
		out += c
	}
	return out
}

// fieldList renders the same columns as the field pointers the fix is written
// with, so the message can be pasted rather than translated.
func fieldList(cols []string) string {
	out := ""
	for i, c := range cols {
		if i > 0 {
			out += ", "
		}
		out += "&m." + goFieldName(c)
	}
	return out
}

// goFieldName turns a snake_case column back into the exported Go field it most
// likely came from. Only ever used inside an error message, where being close
// is worth more than being provably right.
func goFieldName(col string) string {
	out, up := "", true
	for _, r := range col {
		if r == '_' {
			up = true
			continue
		}
		if up && r >= 'a' && r <= 'z' {
			r -= 32
		}
		up = false
		out += string(r)
	}
	return out
}
