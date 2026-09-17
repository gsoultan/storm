package planspike_test

import (
	"context"
	"testing"
	"time"

	"github.com/gsoultan/storm/internal/planspike/store/post"
	"github.com/gsoultan/storm/internal/planspike/store/user"
)

// IS NULL and IS NOT NULL bind no argument, so they must not consume an arena
// slot when the predicate is recorded — the bind loop skips them without
// advancing its cursor, and a leaf that advanced one desynchronised the two.
//
// The symptom is a SILENT WRONG ANSWER, and only when the null check comes
// before another predicate on a column of the SAME arena: everything after it
// read one slot early, so the next predicate was bound to whatever the null
// check had left behind — the zero value. Found against a real schema, where
// `tenant = ? AND uri IS NOT NULL AND status = ?` matched nothing while each
// pair of those three matched correctly.
//
// PublishedAt and CreatedAt are both timestamps, which is what puts them in
// one arena and makes this reproducible.
func TestNullCheckDoesNotConsumeAnArenaSlot(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	// An author from the fixture: this test is about predicate binding, and
	// creating a user would drag in its org and its money columns.
	authors, err := user.New().Limit(1).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(authors) == 0 {
		t.Skip("fixture has no users")
	}
	author := authors[0]

	published := post.Create()
	published.SetTitle("published")
	published.SetBody("b")
	published.SetAuthorID(author.ID)
	published.SetPublishedAt(time.Now().Add(-time.Hour))
	p1, err := published.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	draft := post.Create()
	draft.SetTitle("draft")
	draft.SetBody("b")
	draft.SetAuthorID(author.ID)
	d1, err := draft.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = post.Delete(ctx, ex, p1.ID)
		_ = post.Delete(ctx, ex, d1.ID)
	})

	future := time.Now().Add(time.Hour)

	// Each predicate alone is right, which is why this survived: only the
	// COMBINATION is wrong, and only in this order.
	onlyNotNull, err := post.New().
		Where(post.AuthorID.Eq(author.ID), post.PublishedAt.IsNotNull()).
		Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if onlyNotNull != 1 {
		t.Fatalf("author + published-is-not-null = %d, want 1", onlyNotNull)
	}

	onlyTime, err := post.New().
		Where(post.AuthorID.Eq(author.ID), post.CreatedAt.Lt(future)).
		Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if onlyTime != 2 {
		t.Fatalf("author + created-before-now = %d, want 2", onlyTime)
	}

	// The null check FIRST, then another predicate in the same arena.
	both, err := post.New().
		Where(post.AuthorID.Eq(author.ID),
			post.PublishedAt.IsNotNull(),
			post.CreatedAt.Lt(future)).
		Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if both != 1 {
		t.Fatalf("author + not-null + created-before-now = %d, want 1 — "+
			"the null check consumed the slot the time predicate then read", both)
	}

	// And the other order, which was always right and must stay so.
	reversed, err := post.New().
		Where(post.AuthorID.Eq(author.ID),
			post.CreatedAt.Lt(future),
			post.PublishedAt.IsNotNull()).
		Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if reversed != 1 {
		t.Fatalf("reversed order = %d, want 1", reversed)
	}
}
