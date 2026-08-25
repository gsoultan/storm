// Package model is the quickstart's schema: two tables, one relation, and the
// declared plans and projections this example reads with.
//
// This is the whole model surface a real adopter starts from — plain structs,
// field-pointer declarations, no tags, no DSL. Everything else in the example
// is generated from it.
package model

import (
	"time"

	"github.com/gsoultan/raorm"
)

// Author writes articles.
type Author struct {
	raorm.Model // uuid id + created_at/updated_at, with database defaults

	Name  string
	Email string

	Articles []Article
}

func (a *Author) Schema(t *raorm.Table) {
	t.Col(&a.Email).Size(320)
	t.Unique(&a.Email)
}

// Plans: the reviewable list of every way this example loads an author with
// its relations. One round trip per relation, whatever the row count.
func (a *Author) Plans(p *raorm.Plans) {
	p.Named("Feed").With(&a.Articles)
}

// Projections: the declared column subsets. Card is the list-endpoint read —
// two columns instead of the row, and a covering index away from an
// index-only scan.
func (a *Author) Projections(p *raorm.Projections) {
	p.Named("Card", &a.Name, &a.Email)
}

// Article belongs to an author.
type Article struct {
	raorm.Model

	Title       string
	Body        string
	PublishedAt *time.Time

	Author Author
}

func (ar *Article) Schema(t *raorm.Table) {
	t.Col(&ar.Title).Size(300)
	t.Col(&ar.Author).OnDelete(raorm.Cascade)
}

// All is what Build and the generator consume.
func All() []any { return []any{&Author{}, &Article{}} }
