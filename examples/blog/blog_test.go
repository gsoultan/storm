// The quickstart, as a test CI runs: every claim below executes against a real
// PostgreSQL, so the example cannot drift from the library the way prose does.
// Read it top to bottom — it is the tour.
package blog_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/examples/blog/model"
	"github.com/gsoultan/storm/examples/blog/store"
	"github.com/gsoultan/storm/examples/blog/store/article"
	"github.com/gsoultan/storm/examples/blog/store/author"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	pool *pgxpool.Pool
	ex   runtime.Executor
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		fmt.Println("STORM_DSN unset; skipping the example")
		os.Exit(0)
	}
	ctx := context.Background()

	// The pool comes from storm's constructor so the fast parameter encoders
	// are installed; everything else about it is ordinary pgx.
	var err error
	pool, err = pgxdrv.NewPool(ctx, dsn)
	must(err)
	defer pool.Close()
	ex = pgxdrv.Pool{P: pool}

	// The schema: model → DDL, applied to this example's own namespace. In a
	// real project `storm diff` emits this as a reviewable migration instead —
	// storm itself never applies DDL.
	s, err := storm.Build(model.All()...)
	must(err)
	_, err = pool.Exec(ctx, "DROP SCHEMA IF EXISTS blog_example CASCADE; CREATE SCHEMA blog_example")
	must(err)
	_, err = pool.Exec(ctx, "SET search_path TO blog_example; "+pgddl.Create(s))
	must(err)
	// Scope every pooled connection to the namespace.
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["search_path"] = "blog_example"
	pool.Close()
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	ex = pgxdrv.Pool{P: pool}

	os.Exit(m.Run())
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func TestTheTour(t *testing.T) {
	ctx := context.Background()

	// ---- Writes: a masked insert. Unset columns take their DATABASE
	// defaults — id and created_at come back filled, because absence is
	// tracked by the mask, never inferred from a zero value.
	na := author.Create()
	na.SetName("Ada")
	na.SetEmail("ada@example.com")
	ada, err := na.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if ada.ID == ([16]byte{}) || ada.CreatedAt.IsZero() {
		t.Fatal("database defaults did not come back")
	}

	// ---- A graph write through the unit of work: staged in ANY order,
	// flushed in foreign-key order, one round trip, atomic.
	grace := author.Row{}
	{
		u := store.NewUnit()
		graceID := newID()
		artID := newID()
		u.Add(article.Table, article.InsertOp(article.Row{
			ID: artID, Title: "Compilers", Body: "…", AuthorID: graceID,
		}))
		u.Add(author.Table, author.InsertOp(author.Row{
			ID: graceID, Name: "Grace", Email: "grace@example.com",
		}))
		if _, err := u.Flush(ctx, ex); err != nil {
			t.Fatal(err)
		}
		grace.ID = graceID
	}

	// ---- Publish Ada's two articles; leave Grace's as a draft.
	for i, title := range []string{"On Engines", "Notes"} {
		n := article.Create()
		n.SetTitle(title)
		n.SetBody("…")
		n.SetAuthorID(ada.ID)
		n.SetPublishedAt(time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC))
		if _, err := n.Insert(ctx, ex); err != nil {
			t.Fatal(err)
		}
	}

	// ---- Typed queries: predicates compose; two calls with different values
	// share ONE compiled statement, and a warm build allocates nothing.
	published, err := article.New().
		Where(article.AuthorID.Eq(ada.ID), article.PublishedAt.IsNotNull()).
		Order(article.PublishedAt.Desc()).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 || published[0].Title != "Notes" {
		t.Fatalf("published = %+v", published)
	}

	// ---- The semi-join: authors who HAVE a published article — one EXISTS
	// probe per author, no join fan-out, no DISTINCT.
	n, err := store.AuthorHavingArticles(
		author.New(),
		article.PublishedAt.IsNotNull(),
	).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}

	// ---- And its opposite: the anti-join. Grace HAS an article — it is just
	// not published — so she is absent from the semi-join above and present
	// here. That is the distinction the doc comment warns about: "has no
	// published article" is not "has an unpublished one", and the two differ
	// for anyone holding both.
	if got, err := store.AuthorNotHavingArticles(
		author.New(),
		article.PublishedAt.IsNotNull(),
	).Count(ctx, ex); err != nil {
		t.Fatal(err)
	} else if got != 1 {
		t.Fatalf("authors with no published article = %d, want 1 (Grace)", got)
	}

	// With no child predicate at all it means "has none", and both authors
	// have written something.
	if got, err := store.AuthorNotHavingArticles(author.New()).Count(ctx, ex); err != nil {
		t.Fatal(err)
	} else if got != 0 {
		t.Fatalf("authors with no articles at all = %d, want 0", got)
	}

	// ---- Both probes in ONE statement: "has a published article, and none
	// titled X". This is the upsell query — bought this, never bought that —
	// and doing it in two round trips means intersecting in Go, which is the
	// join the database was going to do anyway.
	//
	// Ada has two published articles, one of them "Notes", so the second probe
	// excludes her. Grace has no published article, so the first excludes her.
	if got, err := store.AuthorHavingArticles(author.New(), article.PublishedAt.IsNotNull()).
		AndNotHaving(article.Title.Eq("Notes")).
		Count(ctx, ex); err != nil {
		t.Fatal(err)
	} else if got != 0 {
		t.Fatalf("published-but-not-Notes authors = %d, want 0 (Ada wrote Notes)", got)
	}

	// The same chain against a title nobody used: Ada qualifies on both probes.
	if got, err := store.AuthorHavingArticles(author.New(), article.PublishedAt.IsNotNull()).
		AndNotHaving(article.Title.Eq("Nothing By This Name")).
		Count(ctx, ex); err != nil {
		t.Fatal(err)
	} else if got != 1 {
		t.Fatalf("published-but-not-Nothing authors = %d, want 1 (Ada)", got)
	}

	// Two POSITIVE probes, ANDed: both must match, and they may match
	// different rows — Ada's two articles, one per probe.
	if got, err := store.AuthorHavingArticles(author.New(), article.Title.Eq("On Engines")).
		AndHaving(article.Title.Eq("Notes")).
		Count(ctx, ex); err != nil {
		t.Fatal(err)
	} else if got != 1 {
		t.Fatalf("authors with both titles = %d, want 1 (Ada)", got)
	}
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d authors have published articles, want 1 (Ada)", n)
	}

	// ---- The named plan: authors WITH their articles, exactly two round
	// trips however many rows — reading an unloaded relation on a bare
	// author.Row does not compile at all.
	count := &runtime.CountingExecutor{Inner: ex}
	feed, err := store.AuthorFeed().Limit(10).All(ctx, count)
	if err != nil {
		t.Fatal(err)
	}
	if got := count.RoundTrips(); got != 2 {
		t.Fatalf("the feed cost %d round trips, want 2", got)
	}
	for _, a := range feed {
		if a.Name == "Ada" && len(a.Articles) != 2 {
			t.Fatalf("Ada's feed carries %d articles, want 2", len(a.Articles))
		}
	}

	// ---- The projection: two columns instead of the row. Same predicates,
	// same ordering, narrower tuple.
	cards, err := author.New().Order(author.Name.Asc()).AllCard(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 || cards[0].Name != "Ada" || cards[0].Email != "ada@example.com" {
		t.Fatalf("cards = %+v", cards)
	}

	// ---- Optimistic reads/writes: update only what was assigned; a stale
	// writer is rejected, not silently last-write-wins.
	m1 := author.Mutate(ada)
	m1.SetName("Ada L.")
	if err := m1.Update(ctx, ex); err != nil {
		t.Fatal(err)
	}

	// ---- The union: two tables merged into one reverse-chronological stream,
	// ordered and paged as a MERGE rather than per source. Two authors and two
	// published articles; Grace's draft is excluded by the branch's declared
	// filter, which no call site can widen.
	stream, err := store.Feed(ctx, ex, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 4 {
		t.Fatalf("feed has %d rows, want 4 (2 authors + 2 published articles)", len(stream))
	}
	kinds := map[string]int{}
	for i, r := range stream {
		kinds[r.Kind]++
		if i > 0 && r.At.After(stream[i-1].At) {
			t.Fatalf("feed is not descending at %d: %s after %s", i, r.At, stream[i-1].At)
		}
	}
	if kinds["author"] != 2 || kinds["article"] != 2 {
		t.Fatalf("feed kinds = %v, want 2 of each", kinds)
	}

	// The limit caps the MERGED result, not each branch — which is the whole
	// difference between a feed and two lists the caller interleaves.
	if page, err := store.Feed(ctx, ex, 3); err != nil {
		t.Fatal(err)
	} else if len(page) != 3 {
		t.Fatalf("limited feed has %d rows, want 3", len(page))
	}

	// ---- Transactions are Executors you were given: the same generated code
	// runs inside one, and a rollback erases everything it did.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	txe := pgxdrv.Tx{T: tx}
	nb := author.Create()
	nb.SetName("Ephemeral")
	nb.SetEmail("gone@example.com")
	if _, err := nb.Insert(ctx, txe); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := author.New().Count(ctx, ex); got != 2 {
		t.Fatalf("%d authors after rollback, want 2", got)
	}

	// ---- Keyset pagination: a cursor in the data, not a page count.
	p1, err := article.New().Order(article.Title.Asc(), article.ID.Asc()).Limit(2).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := article.New().Order(article.Title.Asc(), article.ID.Asc()).
		After(p1[len(p1)-1]).Limit(2).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1)+len(p2) != 3 {
		t.Fatalf("paged %d+%d articles, want all 3 exactly once", len(p1), len(p2))
	}
	_ = grace
}

func newID() [16]byte {
	var v [16]byte
	b := []byte(fmt.Sprintf("%d", time.Now().UnixNano()))
	copy(v[:], b)
	return v
}
