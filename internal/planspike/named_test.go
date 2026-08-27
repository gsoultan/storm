package planspike_test

import (
	"context"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store"
	"github.com/gsoultan/storm/internal/planspike/store/comment"
	"github.com/gsoultan/storm/internal/planspike/store/org"
	"github.com/gsoultan/storm/internal/planspike/store/post"
	"github.com/gsoultan/storm/internal/planspike/store/user"
)

// A named plan loading two relations costs THREE round trips: one for the
// parents and one per relation. Composing two single-relation plans instead
// would cost four, because each would re-read the parents.
func TestNamedPlan_OneRoundTripPerRelation(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	count.Reset()

	rows, err := store.UserFeed().Limit(25).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 25 {
		t.Fatalf("got %d users, want 25", len(rows))
	}
	if n := count.RoundTrips(); n != 3 {
		t.Errorf("%d round trips for a two-relation plan, want 3", n)
	}
	for _, r := range rows {
		if r.Org.ID != r.OrgID {
			t.Fatal("a user is joined to the wrong org")
		}
	}
}

// A one-relation plan is two round trips, and the row type carries only that
// relation — Summary has an Org and no Posts.
func TestNamedPlan_SummaryIsTwoRoundTrips(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	count.Reset()

	rows, err := store.UserSummary().Limit(10).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 10 {
		t.Fatalf("got %d users, want 10", len(rows))
	}
	if n := count.RoundTrips(); n != 2 {
		t.Errorf("%d round trips, want 2", n)
	}
}

// Two has-many relations on one parent, still one query each.
func TestNamedPlan_TwoHasManyRelations(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	count.Reset()

	rows, err := store.OrgTree().Limit(5).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("got %d orgs, want 5", len(rows))
	}
	if n := count.RoundTrips(); n != 3 {
		t.Errorf("%d round trips, want 3", n)
	}
	for _, r := range rows {
		if len(r.Users) != usersPerOrg {
			t.Fatalf("org %s has %d users, want %d", r.Name, len(r.Users), usersPerOrg)
		}
		for _, u := range r.Users {
			if u.OrgID != r.ID {
				t.Fatal("a user was attached to the wrong org")
			}
		}
	}
}

// A named plan pages its parents like any other.
func TestNamedPlan_PagesItsParents(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	p1, err := store.UserSummary().Order(user.Email.Asc(), user.ID.Asc()).Limit(5).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := store.UserSummary().Order(user.Email.Asc(), user.ID.Asc()).
		After(p1[len(p1)-1]).Limit(5).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2) == 0 {
		t.Fatal("no second page")
	}
	for _, a := range p1 {
		for _, b := range p2 {
			if a.ID == b.ID {
				t.Fatal("page two repeats a row from page one")
			}
		}
	}
}

// Predicates apply to the parent, and the relations follow the filtered set —
// not the whole table.
func TestNamedPlan_PredicatesNarrowTheRelations(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)

	one, err := org.New().Limit(1).All(ctx, ex, nil)
	if err != nil || len(one) == 0 {
		t.Fatalf("need an org: %v", err)
	}
	count.Reset()
	rows, err := store.OrgTree().Where(org.ID.Eq(one[0].ID)).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d orgs, want 1", len(rows))
	}
	if n := count.RoundTrips(); n != 3 {
		t.Errorf("%d round trips, want 3", n)
	}
	for _, u := range rows[0].Users {
		if u.OrgID != one[0].ID {
			t.Fatal("the relation load ignored the parent predicate")
		}
	}
}

// A nested plan loads a relation of a relation: users, their posts, and those
// posts' comments. THREE round trips — one per relation plus the parents —
// whatever the row counts. This is also the shape m2m-with-payload takes: the
// join entity is the middle relation and its far side is the nested one.
func TestNamedPlan_NestedIsOneRoundTripPerRelation(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)

	// Seed a small graph: one user, two posts, comments on each.
	u := mustCreate(t, ctx, ex, "nested@example.com", "Nested")
	var postIDs [][16]byte
	for i := range 2 {
		np := post.Create()
		np.SetTitle("post " + string(rune('a'+i)))
		np.SetBody("body")
		np.SetAuthorID(u.ID)
		pr, err := np.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		postIDs = append(postIDs, pr.ID)
		for j := range 3 {
			nc := comment.Create()
			nc.SetBody("comment " + string(rune('a'+j)))
			nc.SetPostID(pr.ID)
			nc.SetAuthorID(u.ID)
			if _, err := nc.Insert(ctx, ex); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() {
		for _, id := range postIDs {
			_ = post.Delete(ctx, ex, id) // comments cascade
		}
		_ = user.Delete(ctx, ex, u.ID)
	})

	count.Reset()
	rows, err := store.UserFeed().Where(user.ID.Eq(u.ID)).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if n := count.RoundTrips(); n != 4 {
		t.Errorf("%d round trips, want 4 — users, posts, comments, orgs", n)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d users, want 1", len(rows))
	}
	if len(rows[0].Posts) != 2 {
		t.Fatalf("got %d posts, want 2", len(rows[0].Posts))
	}
	for _, p := range rows[0].Posts {
		if len(p.Comments) != 3 {
			t.Errorf("post %q has %d comments, want 3", p.Title, len(p.Comments))
		}
		for _, c := range p.Comments {
			if c.PostID != p.ID {
				t.Error("a comment was attached to the wrong post")
			}
		}
	}
}

// The round-trip count must not grow with the row count — that is the whole
// difference between a nested plan and an N+1.
//
// It is CONSTANT, not always four: a level with nothing to fetch issues no
// query, so a plan over users with no posts costs three rather than four. Not
// issuing a guaranteed-empty query is the same rule the empty-parent case
// follows, and asserting a fixed four would have pinned the wrong thing.
func TestNamedPlan_NestedDoesNotScaleWithRowCount(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)

	var first int
	for i, users := range []int64{1, 10, 50, 200} {
		count.Reset()
		if _, err := store.UserFeed().Limit(users).All(ctx, ex); err != nil {
			t.Fatal(err)
		}
		n := count.RoundTrips()
		if n > 4 {
			t.Fatalf("%d users cost %d round trips; a three-relation plan can never exceed 4", users, n)
		}
		// The count may only RISE as the row count grows, and only by skipping
		// fewer empty levels — never once per row.
		//
		// The first version of this asserted a constant, and it was flaky:
		// another test in the package seeds a user with posts, so whether the
		// comment level has anything to fetch depends on which rows this query
		// happened to include. That is legitimate behaviour — an empty level
		// issues no query — and the bound is the property actually worth
		// pinning. A constant assertion pinned the fixture, not the design.
		if n < first {
			t.Errorf("round trips fell from %d to %d as rows grew, which should be impossible", first, n)
		}
		if i == 0 {
			first = n
		}
	}
}
