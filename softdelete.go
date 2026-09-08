package storm

import (
	"fmt"

	"github.com/gsoultan/storm/schema"
)

// Soft delete and uniqueness.
//
// A marked row is still a row. It keeps its primary key and it keeps its place
// in every unique key, so a table that soft-deletes a user and then declares
// UNIQUE (email) has quietly promised that the address can never be used again
// — not by a new signup, and not by the same person coming back. Nothing fails
// at declaration time, nothing fails at migration time, and it fails the first
// time somebody re-registers.
//
// PostgreSQL can only say "unique among the rows that are alive" as a PARTIAL
// unique INDEX; a UNIQUE constraint cannot carry a predicate. So on a
// soft-delete table storm emits every uniqueness declaration as a partial
// unique index over the live rows:
//
//	t.Unique(&u.Email)
//
//	CREATE UNIQUE INDEX uq_users_email ON users (email) WHERE deleted_at IS NULL
//
// A live row and any number of deleted rows may then hold the same value, which
// is what "the record is the same but one is deleted" has to mean for the
// feature to be usable at all. Two LIVE rows still cannot.
//
// This is a rewrite of what the model said, and storm does not do that lightly.
// It is justified here because the alternative reading — "no deleted row may
// ever have shared this value" — is not what any caller means by declaring a
// table soft-deleting and then declaring a unique key on it, and because the
// rewrite is visible: it is in the emitted DDL, in `storm diff`, and in the
// index name. Where the other reading IS meant, UniqueAcrossDeleted says so.
const liveRowsOnly = " IS NULL"

// scopeUniquesToLiveRows rewrites uniqueness on a soft-delete table.
//
// Runs BEFORE index validation, so the indexes it produces are validated like
// any others, and before the foreign-key index pass, so a unique index it
// creates can satisfy that pass.
func (b *builder) scopeUniquesToLiveRows() {
	for _, mi := range b.ordered {
		t := mi.tbl.out
		if !t.SoftDeletes() {
			continue
		}
		pred := t.SoftDelete + liveRowsOnly

		// Constraints become partial unique indexes. The name is the one the
		// constraint would have had, so a table that gains soft delete later
		// does not also rename its keys.
		for _, u := range t.Uniques {
			if mi.tbl.acrossDeleted[uniqueKey(u.Columns)] {
				continue
			}
			cols := make([]schema.IndexColumn, len(u.Columns))
			for i, c := range u.Columns {
				cols[i] = schema.IndexColumn{Name: c}
			}
			name := u.Name
			if name == "" {
				name = t.UniqueName(u)
			}
			t.Indexes = append(t.Indexes, &schema.Index{
				Name: name, Columns: cols, Unique: true, Where: pred,
			})
		}
		t.Uniques = keepAcrossDeleted(mi.tbl, t.Uniques)

		// A unique index declared directly gets the same treatment, unless it
		// already carries a predicate — a declaration that says WHERE means it.
		for _, ix := range t.Indexes {
			if ix.Unique && ix.Where == "" && !mi.tbl.acrossDeletedIx[ix] {
				ix.Where = pred
			}
		}
	}
}

// keepAcrossDeleted returns the uniques that stay real constraints.
func keepAcrossDeleted(t *Table, us []*schema.Unique) []*schema.Unique {
	out := us[:0]
	for _, u := range us {
		if t.acrossDeleted[uniqueKey(u.Columns)] {
			out = append(out, u)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// uniqueKey identifies a declaration by its columns, which is what the caller
// named it with.
func uniqueKey(cols []string) string {
	k := ""
	for _, c := range cols {
		k += c + "\x00"
	}
	return k
}

// validateSoftDelete checks what the rewrite cannot fix.
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
	}
}
