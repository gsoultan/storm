package planspike_test

import (
	"context"
	"testing"

	"github.com/gsoultan/raorm/internal/planspike/store"
	"github.com/gsoultan/raorm/internal/planspike/store/org"
	"github.com/gsoultan/raorm/internal/planspike/store/user"
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
