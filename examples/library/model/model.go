// Package model is a small library — authors and their books — modelled once
// and generated for MongoDB.
//
// The one line that would not be here for a SQL target is OnDocument: a
// document store has no foreign keys, so a reference must say how it is
// stored, and storm refuses to guess (ADR-0004). On PostgreSQL the same model
// keeps its foreign key exactly as declared.
package model

import (
	"time"

	"github.com/gsoultan/storm"
)

// Author writes books.
type Author struct {
	storm.Model

	Name  string
	Email string
	Bio   *string
	Books []Book
}

func (a *Author) Schema(t *storm.Table) {
	t.Col(&a.Email).Size(320)
	t.Unique(&a.Email)
}

// Book belongs to an author.
type Book struct {
	storm.Model

	Title       string
	Pages       int32
	Price       storm.Decimal
	PublishedAt *time.Time
	ISBN        *string

	Author Author
}

func (b *Book) Schema(t *storm.Table) {
	t.Col(&b.Title).Size(300)
	t.Col(&b.Price).Numeric(10, 2)
	t.Col(&b.Author).OnDocument(storm.Reference())
	// Nullable and unique: many books may lack an ISBN, as PostgreSQL allows.
	// compile/mgddl translates this to a partial index so MongoDB agrees.
	t.Unique(&b.ISBN)
}

// All is what Build and the generator consume.
func All() []any { return []any{&Author{}, &Book{}} }
