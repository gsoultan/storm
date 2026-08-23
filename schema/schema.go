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

	PrimaryKey  []string
	Uniques     []*Unique
	Indexes     []*Index
	ForeignKeys []*ForeignKey
	Checks      []*Check
	Excludes    []*Exclude
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
