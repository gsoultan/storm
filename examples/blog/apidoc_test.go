package blog_test

// docs/API.md, compiled.
//
// The document is the first thing an evaluator reads, and until 2026-09-04 it
// described an API that did not exist — `user.Query()`, `storm.Pred`,
// `storm.OnConflict`, a `GroupBy(...).Select(...)` chain that the compilation
// thesis rules out on purpose. It carried an "as-built note" listing some of
// the drift, which is the shape of a document nobody can trust: the reader has
// to know which half is real.
//
// Prose cannot be tested, so this file is the next best thing — every call
// shape API.md shows appears below, against the generated store. A method that
// does not exist fails the build, and a rename that lands in the generated code
// without landing in the document fails it too.
//
// It is deliberately compile-only. The behaviour is already covered by
// blog_test.go against a real server; what is at risk here is the SHAPE.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gsoultan/storm/examples/blog/store"
	"github.com/gsoultan/storm/examples/blog/store/article"
	"github.com/gsoultan/storm/examples/blog/store/author"
	"github.com/gsoultan/storm/runtime"
)

func apiDocSamples(ctx context.Context, ex runtime.Executor, id [16]byte, e string, cutoff time.Time) error {
	// §4 Reads
	rows, err := article.New().
		Where(article.AuthorID.Eq(id), article.PublishedAt.IsNotNull()).
		Order(article.PublishedAt.Desc()).
		Limit(50).
		All(ctx, ex, nil)
	_ = rows

	one, ok, err := author.New().IDEq(id).One(ctx, ex)
	n, err := author.New().Count(ctx, ex)
	ok2, err := author.New().Where(author.Email.Eq(e)).Exists(ctx, ex)
	_, _, _, _ = one, ok, n, ok2

	// §4 keyset pagination
	page1, _ := article.New().Order(article.Title.Asc(), article.ID.Asc()).
		Limit(2).All(ctx, ex, nil)
	if len(page1) > 0 {
		_, _ = article.New().Order(article.Title.Asc(), article.ID.Asc()).
			After(page1[len(page1)-1]).Limit(2).All(ctx, ex, nil)
	}

	// §5 dynamic filters
	q := article.New().Where(article.AuthorID.Eq(id))
	q = q.WhereIf(true, article.PublishedAt.IsNotNull())
	q = q.WhereIf(e != "", article.Title.ILike("%"+e+"%"))
	q = q.Where(article.CreatedAt.Gte(cutoff))
	_, err = q.Order(article.CreatedAt.Desc()).All(ctx, ex, nil)
	_ = article.New().Any(article.Title.Eq("a"), article.Title.Eq("b"))
	_ = article.New().Not(article.Title.Eq("a"))
	_ = article.New().NotAny(article.Title.Eq("a"))

	// §3 typed columns
	_ = article.Title.Like("On %")
	_ = article.Title.Gt("M")
	_ = article.PublishedAt.IsNotNull()

	// §6 plans and the semi-join
	feed, err := store.AuthorFeed().Limit(10).All(ctx, ex)
	if len(feed) > 0 {
		var _ []article.Row = feed[0].Articles
	}
	_, err = store.AuthorHavingArticles(
		author.New(), article.PublishedAt.IsNotNull(),
	).Count(ctx, ex)

	// §7 projections
	var cards []author.CardRow
	cards, err = author.New().Order(author.Name.Asc()).AllCard(ctx, ex)
	_ = cards

	// §8 writes
	na := author.Create()
	na.SetName("Ada")
	na.SetEmail("ada@example.com")
	ada, err := na.Insert(ctx, ex)
	na.OnConflictEmail()

	m := author.Mutate(ada)
	m.SetName("Ada L.")
	err = m.Update(ctx, ex)

	// §9 unit of work
	u := store.NewUnit()
	u.Add(article.Table, article.InsertOp(article.Row{ID: id, Title: "Compilers", AuthorID: ada.ID}))
	u.Add(author.Table, author.InsertOp(author.Row{ID: ada.ID, Name: "Grace", Email: "g@example.com"}))
	_, err = u.Flush(ctx, ex)

	// §11 errors
	if err != nil {
		var ce *runtime.ConstraintError
		if errors.As(err, &ce) {
			switch {
			case errors.Is(err, runtime.ErrUniqueViolation):
				return fmt.Errorf("email %q is taken (%s)", e, ce.Constraint)
			case errors.Is(err, runtime.ErrForeignKeyViolation):
				return err
			case errors.Is(err, runtime.ErrExclusionViolation):
				return err
			}
		}
		if errors.Is(err, runtime.ErrSerializationFailure) || errors.Is(err, runtime.ErrDeadlock) {
			return err
		}
	}
	return err
}

// TestAPIDocSamplesCompile exists so the samples above are referenced rather
// than dead code a linter would offer to delete. Reaching this test at all
// means they compiled, which is the whole assertion.
func TestAPIDocSamplesCompile(t *testing.T) {
	var fn func(context.Context, runtime.Executor, [16]byte, string, time.Time) error = apiDocSamples
	_ = fn
}
