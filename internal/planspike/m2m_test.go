package planspike_test

import (
	"context"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store/org"
	"github.com/gsoultan/storm/internal/planspike/store/user"
	"github.com/gsoultan/storm/runtime"

	"github.com/gsoultan/storm/internal/planspike/store"
	"github.com/gsoultan/storm/internal/planspike/store/post"
	"github.com/gsoultan/storm/internal/planspike/store/posttag"
	"github.com/gsoultan/storm/internal/planspike/store/tag"
)

// m2mAuthor creates an author in its OWN org.
//
// Not mustCreate: that seeds into the org three other tests assert has exactly
// 500 users, and an author added there fails them under -shuffle=on. The same
// mistake the AnyRef test made an hour earlier — a shared fixture is shared in
// both directions.
func m2mAuthor(t *testing.T, ctx context.Context, ex runtime.Executor, email string) [16]byte {
	t.Helper()
	no := org.Create()
	no.SetName("m2m-" + email)
	o, err := no.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	nu := user.Create()
	nu.SetEmail(email)
	nu.SetName("M2M")
	nu.SetStatus("pending")
	nu.SetOrgID(o.ID)
	u, err := nu.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	// Every test in this package cleans up what it creates. A user with posts
	// left behind fails TestHasRelation_IsASemiJoin, which counts users with
	// posts across the whole database and expects to be the only one.
	t.Cleanup(func() {
		_ = user.Delete(ctx, ex, u.ID)
		_ = org.Delete(ctx, ex, o.ID)
	})
	return u.ID
}

// keep registers a post and its link rows for deletion. Deleting the post is
// enough for the links: the generated join table cascades from both ends,
// which is the whole reason that is the default.
func keepPost(t *testing.T, ctx context.Context, ex runtime.Executor, id [16]byte) {
	t.Helper()
	t.Cleanup(func() { _ = post.Delete(ctx, ex, id) })
}

func keepTag(t *testing.T, ctx context.Context, ex runtime.Executor, id [16]byte) {
	t.Helper()
	t.Cleanup(func() { _ = tag.Delete(ctx, ex, id) })
}

// Many-to-many end to end: a slice on both sides, a join table nobody declared,
// and a load whose round-trip count is fixed rather than per parent.
//
// THE GATE: three round trips — parents, link rows, far side — at any counts.
// A loader that fetches tags per post is an N+1 wearing a plan's clothes, and
// the only thing that catches it is counting.
func TestManyToMany_ThreeRoundTrips(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)

	author := m2mAuthor(t, ctx, ex, "m2m@example.com")

	// Six tags shared across twenty posts, so the far side is small and the
	// link rows are many — the shape where fetching per parent would show.
	tags := make([][16]byte, 6)
	for i := range tags {
		n := tag.Create()
		n.SetName(string(rune('a'+i)) + "-tag")
		r, err := n.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		tags[i] = r.ID
		keepTag(t, ctx, ex, r.ID)
	}

	const posts = 20
	want := map[[16]byte]int{}
	for i := 0; i < posts; i++ {
		np := post.Create()
		np.SetTitle("m2m post")
		np.SetBody("b")
		np.SetAuthorID(author)
		pr, err := np.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		// Each post gets three of the six tags.
		for j := 0; j < 3; j++ {
			l := posttag.Create()
			l.SetPostID(pr.ID)
			l.SetTagID(tags[(i+j)%len(tags)])
			if _, err := l.Insert(ctx, ex); err != nil {
				t.Fatal(err)
			}
		}
		want[pr.ID] = 3
		keepPost(t, ctx, ex, pr.ID)
	}

	count.Reset()
	rows, err := store.PostWithTags().Where(post.AuthorID.Eq(author)).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if n := count.RoundTrips(); n != 3 {
		t.Errorf("many-to-many load took %d round trips, want 3", n)
	}
	if len(rows) != posts {
		t.Fatalf("got %d posts, want %d", len(rows), posts)
	}
	for _, r := range rows {
		if got := len(r.Tags); got != want[r.ID] {
			t.Errorf("post %x has %d tags, want %d", r.ID, got, want[r.ID])
		}
	}
}

// The far side is fetched once per DISTINCT child, not once per link. Six tags
// across sixty links must come back as six rows — fetching per link is the row
// multiplication a join would cause and a batch loader exists to avoid.
func TestManyToMany_FarSideIsDeduplicated(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	author := m2mAuthor(t, ctx, ex, "dedup@example.com")
	tg := tag.Create()
	tg.SetName("shared-tag")
	shared, err := tg.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	keepTag(t, ctx, ex, shared.ID)

	const posts = 10
	for i := 0; i < posts; i++ {
		np := post.Create()
		np.SetTitle("dedup post")
		np.SetBody("b")
		np.SetAuthorID(author)
		pr, err := np.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		keepPost(t, ctx, ex, pr.ID)
		l := posttag.Create()
		l.SetPostID(pr.ID)
		l.SetTagID(shared.ID)
		if _, err := l.Insert(ctx, ex); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := store.PostWithTags().Where(post.AuthorID.Eq(author)).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != posts {
		t.Fatalf("got %d posts, want %d", len(rows), posts)
	}
	for _, r := range rows {
		if len(r.Tags) != 1 || r.Tags[0].Name != "shared-tag" {
			t.Errorf("post %x got %d tags", r.ID, len(r.Tags))
		}
	}
}

// Both directions. The join table is symmetric and the two sides were wired
// from one place; a tag must reach its posts as readily as a post its tags.
func TestManyToMany_LoadsFromEitherSide(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	author := m2mAuthor(t, ctx, ex, "both@example.com")
	tg := tag.Create()
	tg.SetName("both-ways")
	tr, err := tg.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	keepTag(t, ctx, ex, tr.ID)
	np := post.Create()
	np.SetTitle("both post")
	np.SetBody("b")
	np.SetAuthorID(author)
	pr, err := np.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	keepPost(t, ctx, ex, pr.ID)
	l := posttag.Create()
	l.SetPostID(pr.ID)
	l.SetTagID(tr.ID)
	if _, err := l.Insert(ctx, ex); err != nil {
		t.Fatal(err)
	}

	back, err := store.TagWithPosts().Where(tag.ID.Eq(tr.ID)).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || len(back[0].Posts) != 1 || back[0].Posts[0].ID != pr.ID {
		t.Fatalf("the inverse side did not load: %+v", back)
	}
}

// An empty parent set costs ONE round trip, not three: there is nothing to
// look up links for. The same rule a direct has-many already follows.
func TestManyToMany_EmptyParentSetIsOneRoundTrip(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)

	count.Reset()
	rows, err := store.PostWithTags().Where(post.Title.Eq("no such post at all")).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows", len(rows))
	}
	if n := count.RoundTrips(); n != 1 {
		t.Errorf("an empty parent set took %d round trips, want 1", n)
	}
}
