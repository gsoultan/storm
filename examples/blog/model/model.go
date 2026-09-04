// Package model is the quickstart's schema: two tables, one relation, and the
// declared plans and projections this example reads with.
//
// This is the whole model surface a real adopter starts from — plain structs,
// field-pointer declarations, no tags, no DSL. Everything else in the example
// is generated from it.
package model

import (
	"time"

	"github.com/gsoultan/storm"
)

// Author writes articles.
type Author struct {
	storm.Model // uuid id + created_at/updated_at, with database defaults

	Name  string
	Email string

	Articles []Article
}

func (a *Author) Schema(t *storm.Table) {
	t.Col(&a.Email).Size(320)
	t.Unique(&a.Email)
}

// Plans: the reviewable list of every way this example loads an author with
// its relations. One round trip per relation, whatever the row count.
func (a *Author) Plans(p *storm.Plans) {
	p.Named("Feed").With(&a.Articles)
}

// Projections: the declared column subsets. Card is the list-endpoint read —
// two columns instead of the row, and a covering index away from an
// index-only scan.
func (a *Author) Projections(p *storm.Projections) {
	p.Named("Card", &a.Name, &a.Email)
}

// Article belongs to an author.
type Article struct {
	storm.Model

	Title       string
	Body        string
	PublishedAt *time.Time

	Author Author
}

func (ar *Article) Schema(t *storm.Table) {
	t.Col(&ar.Title).Size(300)
	t.Col(&ar.Author).OnDelete(storm.Cascade)
}

// Feed merges two tables into one reverse-chronological stream — the shape a
// timeline actually has, and the one thing a per-table query cannot give you:
// ordering and paging that apply to the MERGE rather than to each source.
//
// Narrowed by a declared parameter, so it is one author's feed rather than
// everyone's.
//
// A union has no driving table, so it is declared here as a package-level var
// rather than as a method on Author or Article — neither has more claim to it
// than the other (ADR-0008). The closure runs during Build, which is what lets
// it take field pointers into locals the way a Joins method does.
var Feed = storm.Union("Feed", func(u *storm.UnionSpec) {
	// One declared parameter, used by both branches: the same author reaches
	// each, which is what "this author's feed" has to mean.
	author := u.Param("AuthorID")

	var a Author
	authors := u.From(&a)
	authors.Take(&a.CreatedAt, "At")
	authors.Take(&a.Name, "Text")
	authors.Const("Kind", "author")
	authors.Where(storm.Exprs{}.Eq(&a.ID, author))

	var ar Article
	articles := u.From(&ar)
	articles.Take(&ar.CreatedAt, "At")
	articles.Take(&ar.Title, "Text")
	articles.Const("Kind", "article")
	// A declared filter: drafts are never part of this feed, and no call site
	// can widen that.
	articles.Where(storm.Exprs{}.And(
		storm.Exprs{}.IsNotNull(&ar.PublishedAt),
		storm.Exprs{}.Eq(&ar.Author, author),
	))

	u.OrderDesc("At")
})

// All is what Build and the generator consume.
func All() []any { return []any{&Author{}, &Article{}, Feed} }
