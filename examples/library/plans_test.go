// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package library_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/examples/library/store"
	"github.com/gsoultan/storm/examples/library/store/author"
	"github.com/gsoultan/storm/examples/library/store/book"
	"github.com/gsoultan/storm/examples/library/store/volume"
	"github.com/gsoultan/storm/runtime"
)

// Relation loading: one round trip for the parents and one per relation,
// each a $in over the keys the round before it returned.
func TestPlansLoadRelations(t *testing.T) {
	ctx := context.Background()
	na := author.Create()
	na.SetName("planner")
	na.SetEmail("planner@plans.example.com")
	a, err := na.Insert(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var books []book.Row
	for _, title := range []string{"P1", "P2"} {
		nb := book.Create()
		nb.SetTitle(title)
		nb.SetPages(10)
		nb.SetPrice(runtime.Decimal{Unscaled: 1, Scale: 2})
		nb.SetAuthorID(a.ID)
		r, err := nb.Insert(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		books = append(books, r)
	}
	for _, code := range []string{"p1", "p2"} {
		v := volume.Row{Branch: "plans", Barcode: code, BookID: books[0].ID}
		if err := volume.Insert(ctx, db, &v); err != nil {
			t.Fatal(err)
		}
	}
	// A copy of a book that is not there: MongoDB enforces no reference, so
	// a to-one load answers nil rather than failing.
	orphan := volume.Row{Branch: "plans", Barcode: "p3", BookID: runtime.NewUUIDv7()}
	if err := volume.Insert(ctx, db, &orphan); err != nil {
		t.Fatal(err)
	}

	shelf, err := store.AuthorShelf().Where(author.ID.Eq(a.ID)).All(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(shelf) != 1 || len(shelf[0].Books) != 2 {
		t.Fatalf("shelf: %+v", shelf)
	}
	for _, b := range shelf[0].Books {
		want := map[string]int{"P1": 2, "P2": 0}[b.Title]
		if len(b.Volumes) != want {
			t.Errorf("%s has %d volumes, want %d", b.Title, len(b.Volumes), want)
		}
	}

	vs, err := store.VolumeWithBook().Where(volume.Branch.Eq("plans")).Order(volume.Barcode.Asc()).All(ctx, db)
	if err != nil || len(vs) != 3 {
		t.Fatalf("volumes: %d %v", len(vs), err)
	}
	if vs[0].Book == nil || vs[0].Book.Title != "P1" || vs[2].Book != nil {
		t.Fatalf("to-one: %+v %+v", vs[0].Book, vs[2].Book)
	}

	if _, err := store.AuthorWithBooks().Where(author.ID.Eq(a.ID)).ChildLimit(2).All(ctx, db); !errors.Is(err, runtime.ErrChildLimit) {
		t.Fatalf("a load at its limit is an error, not a partial result: %v", err)
	}
	if none, err := store.BookWithAuthor().Where(book.Title.Eq("no such title")).All(ctx, db); err != nil || none != nil {
		t.Fatalf("no parents is one round trip and nil: %v %v", none, err)
	}
	bw, err := store.BookWithAuthor().Where(book.ID.Eq(books[1].ID)).All(ctx, db)
	if err != nil || len(bw) != 1 || bw[0].Author == nil || bw[0].Author.Name != "planner" {
		t.Fatalf("book with author: %+v %v", bw, err)
	}
}

// A projection reads the fields it names and nothing else, with the same
// filter, sort and paging.
func TestProjection(t *testing.T) {
	ctx := context.Background()
	for i, title := range []string{"Card A", "Card B", "Card C"} {
		nb := book.Create()
		nb.SetTitle(title)
		nb.SetPages(int32(100 + i))
		nb.SetPrice(runtime.Decimal{Unscaled: 1, Scale: 2})
		nb.SetAuthorID(runtime.NewUUIDv7())
		if _, err := nb.Insert(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	cards, err := book.New().Where(book.Title.HasPrefix("Card ")).Order(book.Title.Desc()).Limit(2).AllCard(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 || cards[0].Title != "Card C" || cards[0].Pages != 102 {
		t.Fatalf("cards: %+v", cards)
	}
	if c, ok, err := book.New().Where(book.Title.Eq("no such")).OneCard(ctx, db); err != nil || ok {
		t.Fatalf("OneCard of nothing: %+v %v %v", c, ok, err)
	}
	if !strings.Contains(book.New().SQL(), `"tags"`) {
		t.Fatal("the full read projects every field")
	}
}
