// Package schema is raorm's schema IR: the single representation every front
// end produces and every back end consumes.
//
// Front ends: the Go model (model), a live database (schema/pg), migration
// files. Back ends: DDL emission (compile/pgddl), the query compiler, and the
// migration differ (migrate).
//
// Everything here is stdlib-only and deterministic: two runs over the same
// input produce byte-identical output, which is what lets `raorm verify` fail
// CI on a plain diff.
package schema

import (
	"cmp"
	"slices"
	"strings"
)

// Schema is a whole database as raorm understands it.
type Schema struct {
	Tables []*Table
	Enums  []*Enum
}

// Table is one relation.
type Table struct {
	Name    string
	Comment string

	// GoName is the model type this table was declared from — "User" for
	// table "users". It is a raorm-level fact with no DDL of its own, like
	// Column.Immutable, and it exists because pluralisation is not
	// invertible: tableName() turns User into users by naive rules English
	// does not actually follow, so codegen must not try to turn users back
	// into User. Empty when the table came from introspection, where there is
	// no Go type to remember.
	GoName string

	// Columns are kept in declaration order: DDL column order is observable
	// (SELECT *, COPY) so it must not be sorted.
	Columns []*Column

	// Relations are the Go-level links the model declared, kept because a
	// foreign key alone cannot reconstruct them: it says users.org_id
	// references orgs.id, not that the field is called Org, that Org has a
	// Users slice pointing back, or which side the model considers the owner.
	// Code generation needs all three.
	//
	// This is a raorm-level fact with no DDL of its own; ForeignKeys is what
	// the database sees.
	Relations []*Relation

	// Arcs are the exclusive-arc polymorphic fields: one column per variant
	// with a CHECK that exactly one is set. Kept in the IR because code
	// generation needs the variant list, which the columns alone cannot give
	// back — three nullable foreign keys look like three ordinary relations.
	Arcs []*Arc

	// Projections are the named column subsets the model declared. Named, not
	// inferred, for R3's reason: you get the projections you use, never 2^n.
	// Each one exists so a read can fetch LESS — narrower tuples, no TOAST
	// fetch for the jsonb nobody asked for, and the possibility of an
	// index-only scan, which the full-row read forecloses by construction.
	Projections []*Projection

	// Plans are the named fetch plans the model declared. The generator emits
	// exactly these and no others — which is the whole answer to the
	// projection-type explosion: you get the plans you name, not every subset
	// of the relations you have.
	Plans []*Plan

	PrimaryKey  []string
	Uniques     []*Unique
	Indexes     []*Index
	ForeignKeys []*ForeignKey
	Checks      []*Check
	Excludes    []*Exclude
}

// Arc is one polymorphic field: a reference to a row in exactly one of several
// tables, expressed as one nullable foreign key per variant.
type Arc struct {
	// Field is the Go field name: "Subject".
	Field string

	// Optional makes the CHECK "at most one" rather than "exactly one".
	Optional bool

	// Variants are the possible targets, in declaration order. Order is the
	// generated match arity, so changing it changes every call site — which is
	// the point.
	Variants []ArcVariant
}

// ArcVariant is one target of an arc.
type ArcVariant struct {
	Table  string // referenced table
	GoName string // referenced model type
	Column string // this table's nullable foreign key for that variant
}

// Projection is one named column subset.
type Projection struct {
	Name    string
	Columns []string // declaration order: it becomes the row type's field order
}

// Plan is a named fetch plan: a set of relations loaded together.
type Plan struct {
	// Name is the plan's name as declared, e.g. "Feed". The generated type is
	// the model name plus this, e.g. UserFeed.
	Name string

	// Fields are the relations to load, in declaration order. Order is
	// preserved because it is the order the loads are issued, and a reviewer
	// counting round trips should be able to read them off.
	Fields []PlanField
}

// PlanField is one relation in a plan, with anything loaded through it.
type PlanField struct {
	// Field is the Go field name on the plan's model: "Posts".
	Field string

	// Nested are relations loaded through this one — a post's comments, or the
	// far side of a join table. Each costs one more round trip, and the count
	// is what a reviewer should be able to read off a plan.
	Nested []PlanField
}

// Relation is one declared link between two models.
type Relation struct {
	// Field is the Go field name on this table's model: "Org", "Users".
	Field string

	// Target is the table referenced. TargetGo is its model type name, which
	// is what the generated package for it is named after.
	Target   string
	TargetGo string

	// ToMany distinguishes `Users []User` from `Org Org`.
	ToMany bool

	// Column is the foreign-key column. On the owning side it is a column of
	// this table; on the inverse side it is a column of the Target, which is
	// what a batch loader filters on.
	Column string

	// Owner says whether this side carries the key. Exactly one side of a link
	// does — for one-to-one the required side, decided at build time — and the
	// generator loads an owned relation and an inverse relation differently.
	Owner bool

	// Nullable is true when the link is optional (`*Profile`, a nullable FK).
	Nullable bool
}

// Column is one attribute.
type Column struct {
	Name      string
	Type      Type
	NotNull   bool
	Default   string // raw SQL expression; "" for none
	Generated string // GENERATED ALWAYS AS (<expr>) STORED; "" for none
	Identity  bool
	Comment   string

	// Immutable and Version are raorm-level facts with no DDL of their own.
	// They change what the generator emits, not what the database enforces.
	Immutable bool
	Version   bool
}

// Unique is a table-level uniqueness constraint.
type Unique struct {
	Name    string
	Columns []string // column names or expressions
}

// Index is a secondary index.
type Index struct {
	Name    string
	Columns []IndexColumn
	Unique  bool
	Method  string // "btree" when empty
	Where   string // partial-index predicate; "" for none
}

// IndexColumn is one index key. Expr distinguishes lower(email) from a column
// literally named "lower(email)".
type IndexColumn struct {
	Name      string
	Expr      bool
	Desc      bool
	NullsLast bool
}

// ForeignKey is a referential constraint.
type ForeignKey struct {
	Name       string
	Columns    []string
	RefTable   string
	RefColumns []string
	OnDelete   Action
	OnUpdate   Action
	Deferrable bool
}

// Check is a table-level CHECK constraint.
type Check struct {
	Name string
	Expr string
}

// Exclude is an exclusion constraint — the correct answer to booking and
// scheduling overlap, and unreachable from every other Go ORM.
type Exclude struct {
	Name   string
	Method string // "gist" when empty
	Parts  []ExcludePart
	Where  string
}

// ExcludePart is one `<column> WITH <operator>` element.
type ExcludePart struct {
	Column   string
	Expr     bool
	Operator string
}

// Enum is a native Postgres enum type.
type Enum struct {
	Name   string
	Labels []string // order is significant: it defines the sort order
}

// Action is a referential action.
type Action string

const (
	NoAction   Action = ""
	Restrict   Action = "RESTRICT"
	Cascade    Action = "CASCADE"
	SetNull    Action = "SET NULL"
	SetDefault Action = "SET DEFAULT"
)

// ---- lookup helpers ----

func (s *Schema) Table(name string) *Table {
	for _, t := range s.Tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}

func (s *Schema) Enum(name string) *Enum {
	for _, e := range s.Enums {
		if e.Name == name {
			return e
		}
	}
	return nil
}

func (t *Table) Column(name string) *Column {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Normalize puts a schema into canonical form so two schemas built by
// different front ends compare equal. Columns keep declaration order;
// everything else is sorted by name, and unnamed constraints are given
// deterministic generated names first.
func (s *Schema) Normalize() {
	slices.SortStableFunc(s.Tables, func(a, b *Table) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortStableFunc(s.Enums, func(a, b *Enum) int { return cmp.Compare(a.Name, b.Name) })
	for _, t := range s.Tables {
		t.normalize()
	}
}

func (t *Table) normalize() {
	for i, u := range t.Uniques {
		if u.Name == "" {
			u.Name = genName("uq", t.Name, u.Columns...)
		}
		t.Uniques[i] = u
	}
	for i, ix := range t.Indexes {
		if ix.Method == "" {
			ix.Method = "btree"
		}
		if ix.Name == "" {
			cols := make([]string, len(ix.Columns))
			for j, c := range ix.Columns {
				cols[j] = c.Name
			}
			ix.Name = genName("ix", t.Name, cols...)
		}
		t.Indexes[i] = ix
	}
	for i, fk := range t.ForeignKeys {
		if fk.Name == "" {
			fk.Name = genName("fk", t.Name, fk.Columns...)
		}
		t.ForeignKeys[i] = fk
	}
	for i, ck := range t.Checks {
		if ck.Name == "" {
			ck.Name = genName("ck", t.Name, sanitize(ck.Expr))
		}
		t.Checks[i] = ck
	}
	for i, ex := range t.Excludes {
		if ex.Method == "" {
			ex.Method = "gist"
		}
		if ex.Name == "" {
			cols := make([]string, len(ex.Parts))
			for j, p := range ex.Parts {
				cols[j] = p.Column
			}
			ex.Name = genName("ex", t.Name, cols...)
		}
		t.Excludes[i] = ex
	}

	slices.SortStableFunc(t.Uniques, func(a, b *Unique) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortStableFunc(t.Indexes, func(a, b *Index) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortStableFunc(t.ForeignKeys, func(a, b *ForeignKey) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortStableFunc(t.Checks, func(a, b *Check) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortStableFunc(t.Excludes, func(a, b *Exclude) int { return cmp.Compare(a.Name, b.Name) })
}

// genName builds a deterministic constraint name, truncated to Postgres' 63
// byte identifier limit without ever colliding silently: the truncation keeps
// a hash suffix.
func genName(kind, table string, parts ...string) string {
	var b strings.Builder
	b.WriteString(kind)
	b.WriteByte('_')
	b.WriteString(table)
	for _, p := range parts {
		b.WriteByte('_')
		b.WriteString(sanitize(p))
	}
	n := b.String()
	if len(n) <= 63 {
		return n
	}
	return n[:54] + "_" + hash8(n)
}

func sanitize(s string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// hash8 is FNV-1a rendered as 8 hex digits. Stdlib-only by hand so the schema
// package keeps zero imports beyond cmp/slices/strings.
func hash8(s string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = hex[h&0xf]
		h >>= 4
	}
	return string(out)
}
