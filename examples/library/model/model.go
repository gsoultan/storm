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

// Author writes books. Authors are soft-deleted: a removed author keeps
// their document, and every read and write generated for this collection
// treats it as gone. The unique email is scoped to the live authors, so a
// returning author can sign up again with the same address.
type Author struct {
	storm.Model

	Name      string
	Email     string
	Bio       *string
	DeletedAt *time.Time
	Books     []Book
}

func (a *Author) Schema(t *storm.Table) {
	t.Col(&a.Email).Size(320)
	t.Unique(&a.Email)
	t.SoftDelete(&a.DeletedAt).ReadsDeleted()
}

// BookAttrs is a json column: stored as a BSON document, so its fields are
// queryable as what they are.
type BookAttrs struct {
	Format   string `json:"format,omitempty"`
	Language string `json:"language,omitempty"`
	Signed   bool   `json:"signed,omitempty"`
}

// Book belongs to an author.
type Book struct {
	storm.Model

	Title       string
	Pages       int32
	Price       storm.Decimal
	PublishedAt *time.Time
	ISBN        *string
	// Tags is an array, stored as a BSON array; Attrs a json document.
	Tags  []string
	Attrs BookAttrs

	Author Author
}

func (b *Book) Schema(t *storm.Table) {
	t.Col(&b.Title).Size(300)
	t.Col(&b.Price).Numeric(10, 2)
	t.Col(&b.Author).OnDocument(storm.Reference())
	// Nullable and unique: many books may lack an ISBN, as PostgreSQL allows.
	// compile/mgddl translates this to a partial index so MongoDB agrees.
	t.Unique(&b.ISBN)
	t.Col(&b.Tags).Default("'{}'")
	t.Col(&b.Attrs).Default("'{}'")
	// GIN on PostgreSQL; a multikey index and a wildcard index here.
	t.Index(&b.Tags).Using("gin")
	t.Index(&b.Attrs).Using("gin")
}

// Volume is one physical copy of a book at a branch. Its key is composite —
// the branch and the barcode — and is stored as the _id subdocument.
//
// Version makes every update an optimistic lock, and Checkouts counts a
// checkout only when the copy was on the shelf: the test and the increment
// are one write, so two desks racing for one copy count it once.
type Volume struct {
	Branch    string
	Barcode   string
	BookID    storm.UUID
	OnShelf   bool
	Checkouts int64
	Version   int32
}

func (c *Volume) Schema(t *storm.Table) {
	t.PrimaryKey(&c.Branch, &c.Barcode)
	t.Col(&c.Branch).Size(16)
	t.Col(&c.Barcode).Size(32)
	t.Col(&c.Version).Version().Default("0")
	t.Col(&c.Checkouts).IncWhen(&c.OnShelf).Default("0")
	var b Book
	t.ForeignKey(&c.BookID).References(&b, &b.ID).OnDocument(storm.Reference())
}

// Hold is a reader's place in the queue for one copy: a composite reference
// to a composite key.
type Hold struct {
	storm.Model

	Branch  string
	Barcode string
	Reader  string
}

func (h *Hold) Schema(t *storm.Table) {
	t.Col(&h.Branch).Size(16)
	t.Col(&h.Barcode).Size(32)
	var c Volume
	t.ForeignKey(&h.Branch, &h.Barcode).References(&c, &c.Branch, &c.Barcode).
		OnDocument(storm.Reference())
}

// All is what Build and the generator consume.
func All() []any { return []any{&Author{}, &Book{}, &Volume{}, &Hold{}} }
