package library_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gsoultan/storm/examples/library/store"
	"github.com/gsoultan/storm/examples/library/store/author"
	"github.com/gsoultan/storm/examples/library/store/book"
	"github.com/gsoultan/storm/examples/library/store/review"
	"github.com/gsoultan/storm/runtime"
)

// Embedded rows: a book's reviews live in the book's document.
func TestEmbeddedReviews(t *testing.T) {
	ctx := context.Background()
	na := author.Create()
	na.SetName("reviewed")
	na.SetEmail("reviewed@embed.example.com")
	a, err := na.Insert(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	nb := book.Create()
	nb.SetTitle("Embedded")
	nb.SetPages(1)
	nb.SetPrice(runtime.Decimal{Unscaled: 1, Scale: 2})
	nb.SetAuthorID(a.ID)
	b, err := nb.Insert(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	first := review.Row{BookID: b.ID, Reader: "ana", Stars: 4}
	if err := review.Add(ctx, db, &first); err != nil {
		t.Fatal(err)
	}
	if runtime.IsZeroUUID(first.ID) || first.CreatedAt.IsZero() || first.BookID != b.ID || first.Body.Valid {
		t.Fatalf("Add reads the element back, server clock included: %+v", first)
	}
	second := review.Row{BookID: b.ID, Reader: "bo", Stars: 2, Body: runtime.Null[string]{V: "$not a path", Valid: true}}
	if err := review.Add(ctx, db, &second); err != nil {
		t.Fatal(err)
	}
	dup := first
	if err := review.Add(ctx, db, &dup); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Fatalf("a key the parent holds: %v", err)
	}
	orphan := review.Row{BookID: runtime.NewUUIDv7(), Reader: "x", Stars: 1}
	if err := review.Add(ctx, db, &orphan); !errors.Is(err, runtime.ErrForeignKeyViolation) {
		t.Fatalf("no parent: %v", err)
	}
	got, err := review.Of(ctx, db, b.ID)
	if err != nil || len(got) != 2 || got[1].Body.V != "$not a path" || got[0].ID != first.ID {
		t.Fatalf("Of: %+v %v", got, err)
	}

	second.Stars = 5
	if err := review.Replace(ctx, db, second); err != nil {
		t.Fatal(err)
	}
	if err := review.Remove(ctx, db, b.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := review.Remove(ctx, db, b.ID, first.ID); !errors.Is(err, runtime.ErrNoRow) {
		t.Fatalf("removing twice: %v", err)
	}
	ghost := first
	if err := review.Replace(ctx, db, ghost); !errors.Is(err, runtime.ErrNoRow) {
		t.Fatalf("replacing what is gone: %v", err)
	}

	plan, err := store.AuthorReviewed().Where(author.ID.Eq(a.ID)).All(ctx, db)
	if err != nil || len(plan) != 1 || len(plan[0].Books) != 1 || len(plan[0].Books[0].Reviews) != 1 ||
		plan[0].Books[0].Reviews[0].Stars != 5 {
		t.Fatalf("a plan through the embedded relation: %+v %v", plan, err)
	}
	by, err := review.Load(ctx, db, b.ID, runtime.NewUUIDv7())
	if err != nil || len(by) != 1 || len(by[b.ID]) != 1 {
		t.Fatalf("Load: %+v %v", by, err)
	}

	// The book's own reads do not carry the array, and deleting the book
	// takes its reviews with it.
	if err := book.Delete(ctx, db, b.ID); err != nil {
		t.Fatal(err)
	}
	if left, err := review.Of(ctx, db, b.ID); err != nil || len(left) != 0 {
		t.Fatalf("the reviews outlived their book: %+v %v", left, err)
	}
}
