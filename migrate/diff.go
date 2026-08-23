// Package migrate diffs two schemas and emits a reviewable migration.
//
// raorm never applies DDL (ADR-0001). It writes a numbered, forward-only file
// for your migration runner, and marks every step that could lose data so a
// destructive change cannot arrive unannounced.
package migrate

import (
	"fmt"
	"strings"

	"github.com/gsoultan/raorm/compile/pgddl"
	"github.com/gsoultan/raorm/schema"
)

// Change is one migration step.
type Change struct {
	SQL string
	// Destructive means the step can lose data or break a running deployment:
	// dropping an object, narrowing a type, or adding a NOT NULL column with no
	// default to a table that may already have rows.
	Destructive bool
	// Why explains a destructive step, and is emitted as a comment.
	Why string
}

// Plan is an ordered set of changes.
type Plan struct {
	Changes []Change
}

// Empty reports whether the two schemas already agree.
func (p Plan) Empty() bool { return len(p.Changes) == 0 }

// Destructive reports whether any step needs --allow-destructive.
func (p Plan) Destructive() bool {
	for _, c := range p.Changes {
		if c.Destructive {
			return true
		}
	}
	return false
}

// SQL renders the plan as a migration file.
func (p Plan) SQL() string {
	var b strings.Builder
	for _, c := range p.Changes {
		if c.Destructive {
			b.WriteString("-- raorm:destructive " + c.Why + "\n")
		}
		b.WriteString(c.SQL + "\n")
	}
	return b.String()
}

// Diff computes the changes that take `from` to `to`. Both are normalised
// first, so the result does not depend on declaration order.
func Diff(from, to *schema.Schema) Plan {
	if from == nil {
		from = &schema.Schema{}
	}
	if to == nil {
		to = &schema.Schema{}
	}
	from.Normalize()
	to.Normalize()

	var p Plan

	// Enums before tables that use them.
	for _, e := range to.Enums {
		old := from.Enum(e.Name)
		if old == nil {
			p.add(Change{SQL: pgddl.CreateEnum(e)})
			continue
		}
		// Labels can be appended but never removed or reordered in place.
		for _, l := range e.Labels {
			if !contains(old.Labels, l) {
				p.add(Change{SQL: fmt.Sprintf("ALTER TYPE %s ADD VALUE %s;",
					pgddl.Ident(e.Name), quote(l))})
			}
		}
		for _, l := range old.Labels {
			if !contains(e.Labels, l) {
				p.add(Change{
					SQL:         fmt.Sprintf("-- cannot remove enum label %s from %s: PostgreSQL has no DROP VALUE", quote(l), e.Name),
					Destructive: true,
					Why:         "enum label " + l + " removed from the model; recreate the type by hand",
				})
			}
		}
	}

	// New and changed tables. Foreign keys for new tables are held back until
	// every table exists — a plan that adds comments.post_id before posts is
	// created is a plan that fails halfway through a deployment.
	var deferredFKs []Change
	for _, t := range to.Tables {
		old := from.Table(t.Name)
		if old == nil {
			p.add(Change{SQL: strings.TrimRight(pgddl.CreateTable(t), "\n")})
			for _, fk := range t.ForeignKeys {
				deferredFKs = append(deferredFKs, Change{SQL: pgddl.AddForeignKey(t, fk)})
			}
			continue
		}
		diffTable(&p, old, t)
	}
	p.Changes = append(p.Changes, deferredFKs...)

	// Dropped tables, after the rest so foreign keys pointing at them are gone.
	for _, t := range from.Tables {
		if to.Table(t.Name) == nil {
			p.add(Change{
				SQL:         "DROP TABLE " + pgddl.Ident(t.Name) + ";",
				Destructive: true,
				Why:         "table " + t.Name + " is no longer in the model",
			})
		}
	}
	// Dropped enums last: a table using one may have just been dropped.
	for _, e := range from.Enums {
		if to.Enum(e.Name) == nil {
			p.add(Change{
				SQL:         "DROP TYPE " + pgddl.Ident(e.Name) + ";",
				Destructive: true,
				Why:         "enum " + e.Name + " is no longer in the model",
			})
		}
	}
	return p
}

func diffTable(p *Plan, old, cur *schema.Table) {
	q := pgddl.Ident(cur.Name)

	// Columns added.
	for _, c := range cur.Columns {
		oc := old.Column(c.Name)
		if oc == nil {
			ch := Change{SQL: "ALTER TABLE " + q + " ADD COLUMN " + pgddl.ColumnDef(c) + ";"}
			if c.NotNull && c.Default == "" && c.Generated == "" && !c.Identity {
				ch.Destructive = true
				ch.Why = "adding NOT NULL column " + c.Name + " with no default fails if the table has rows"
			}
			p.add(ch)
			continue
		}
		diffColumn(p, q, oc, c)
	}
	// Columns removed.
	for _, oc := range old.Columns {
		if cur.Column(oc.Name) == nil {
			p.add(Change{
				SQL:         "ALTER TABLE " + q + " DROP COLUMN " + pgddl.Ident(oc.Name) + ";",
				Destructive: true,
				Why:         "column " + cur.Name + "." + oc.Name + " is no longer in the model",
			})
		}
	}

	diffNamed(p, cur, old.Uniques, cur.Uniques,
		func(u *schema.Unique) string { return u.Name },
		func(u *schema.Unique) string {
			return "ALTER TABLE " + q + " ADD CONSTRAINT " + pgddl.Ident(u.Name) +
				" UNIQUE (" + identList(u.Columns) + ");"
		},
		func(u *schema.Unique) string {
			return "ALTER TABLE " + q + " DROP CONSTRAINT " + pgddl.Ident(u.Name) + ";"
		},
		func(a, b *schema.Unique) bool { return eq(a.Columns, b.Columns) },
		"unique constraint")

	diffNamed(p, cur, old.Checks, cur.Checks,
		func(c *schema.Check) string { return c.Name },
		func(c *schema.Check) string {
			return "ALTER TABLE " + q + " ADD CONSTRAINT " + pgddl.Ident(c.Name) + " CHECK (" + c.Expr + ");"
		},
		func(c *schema.Check) string {
			return "ALTER TABLE " + q + " DROP CONSTRAINT " + pgddl.Ident(c.Name) + ";"
		},
		func(a, b *schema.Check) bool { return canonical(a.Expr) == canonical(b.Expr) },
		"check constraint")

	diffNamed(p, cur, old.ForeignKeys, cur.ForeignKeys,
		func(f *schema.ForeignKey) string { return f.Name },
		func(f *schema.ForeignKey) string { return pgddl.AddForeignKey(cur, f) },
		func(f *schema.ForeignKey) string {
			return "ALTER TABLE " + q + " DROP CONSTRAINT " + pgddl.Ident(f.Name) + ";"
		},
		func(a, b *schema.ForeignKey) bool {
			return eq(a.Columns, b.Columns) && a.RefTable == b.RefTable &&
				eq(a.RefColumns, b.RefColumns) && a.OnDelete == b.OnDelete && a.OnUpdate == b.OnUpdate
		},
		"foreign key")

	diffNamed(p, cur, old.Indexes, cur.Indexes,
		func(i *schema.Index) string { return i.Name },
		func(i *schema.Index) string { return pgddl.CreateIndex(cur, i) },
		func(i *schema.Index) string { return "DROP INDEX " + pgddl.Ident(i.Name) + ";" },
		func(a, b *schema.Index) bool {
			return a.Unique == b.Unique && a.Method == b.Method &&
				canonical(a.Where) == canonical(b.Where) && sameKeys(a.Columns, b.Columns)
		},
		"index")
}

func diffColumn(p *Plan, q string, old, cur *schema.Column) {
	col := pgddl.Ident(cur.Name)
	if !old.Type.Equal(cur.Type) {
		ch := Change{SQL: "ALTER TABLE " + q + " ALTER COLUMN " + col +
			" TYPE " + cur.Type.SQL() + ";"}
		if narrowing(old.Type, cur.Type) {
			ch.Destructive = true
			ch.Why = "narrowing " + cur.Name + " from " + old.Type.SQL() + " to " + cur.Type.SQL() + " can truncate"
		}
		p.add(ch)
	}
	if old.NotNull != cur.NotNull {
		if cur.NotNull {
			p.add(Change{
				SQL:         "ALTER TABLE " + q + " ALTER COLUMN " + col + " SET NOT NULL;",
				Destructive: true,
				Why:         "SET NOT NULL on " + cur.Name + " fails if any existing row is NULL",
			})
		} else {
			p.add(Change{SQL: "ALTER TABLE " + q + " ALTER COLUMN " + col + " DROP NOT NULL;"})
		}
	}
	if old.Default != cur.Default {
		if cur.Default == "" {
			p.add(Change{SQL: "ALTER TABLE " + q + " ALTER COLUMN " + col + " DROP DEFAULT;"})
		} else {
			p.add(Change{SQL: "ALTER TABLE " + q + " ALTER COLUMN " + col +
				" SET DEFAULT " + cur.Default + ";"})
		}
	}
}

// diffNamed is the add/drop/replace loop shared by every named constraint kind.
func diffNamed[T any](p *Plan, t *schema.Table, old, cur []T,
	name func(T) string, add func(T) string, drop func(T) string,
	same func(a, b T) bool, kind string) {

	byName := map[string]T{}
	for _, o := range old {
		byName[name(o)] = o
	}
	seen := map[string]bool{}
	for _, c := range cur {
		seen[name(c)] = true
		o, ok := byName[name(c)]
		if !ok {
			p.add(Change{SQL: add(c)})
			continue
		}
		if !same(o, c) {
			// Postgres cannot alter these in place; drop and recreate.
			p.add(Change{SQL: drop(o)})
			p.add(Change{SQL: add(c)})
		}
	}
	for _, o := range old {
		if !seen[name(o)] {
			p.add(Change{
				SQL:         drop(o),
				Destructive: true,
				Why:         kind + " " + name(o) + " on " + t.Name + " is no longer in the model",
			})
		}
	}
}

func (p *Plan) add(c Change) { p.Changes = append(p.Changes, c) }

// narrowing reports whether a type change can lose information.
func narrowing(from, to schema.Type) bool {
	if from.Name != to.Name {
		return true // any cross-type change needs a human
	}
	if from.Size > 0 && to.Size > 0 && to.Size < from.Size {
		return true
	}
	if from.Size > 0 && to.Size == 0 {
		return false // widening to unbounded
	}
	if from.Precision > 0 && to.Precision > 0 &&
		(to.Precision < from.Precision || to.Scale < from.Scale) {
		return true
	}
	return false
}

// canonical strips whitespace and outer parens so two spellings of the same
// expression compare equal. Postgres rewrites everything it stores, so exact
// text comparison would report differences that are not real.
func canonical(s string) string {
	s = stripCasts(s)
	s = strings.Join(strings.Fields(s), " ")
	for len(s) > 1 && s[0] == '(' && s[len(s)-1] == ')' {
		depth, ok := 0, true
		for i := 0; i < len(s); i++ {
			if s[i] == '(' {
				depth++
			} else if s[i] == ')' {
				depth--
				if depth == 0 && i != len(s)-1 {
					ok = false
				}
			}
		}
		if !ok {
			break
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return strings.ToLower(strings.ReplaceAll(s, " ", ""))
}

func sameKeys(a, b []schema.IndexColumn) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if canonical(a[i].Name) != canonical(b[i].Name) ||
			a[i].Desc != b[i].Desc || a[i].NullsLast != b[i].NullsLast {
			return false
		}
	}
	return true
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func identList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = pgddl.Ident(n)
	}
	return strings.Join(out, ", ")
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// stripCasts removes ::type suffixes. PostgreSQL adds them to everything it
// stores ('pending' becomes 'pending'::status), and they never change meaning.
// This makes offline diffs — model against a checked-in snapshot — far less
// noisy. It is not a substitute for Normalize: only PostgreSQL knows that
// BETWEEN is two comparisons.
func stripCasts(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == ':' && i+1 < len(s) && s[i+1] == ':' {
			i += 2
			for i < len(s) {
				c := s[i]
				isIdent := c == '_' || c == '.' || c == ' ' ||
					c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
				if !isIdent {
					break
				}
				i++
			}
			i--
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
