package planspike_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gsoultan/raorm/internal/planspike/store"
	"github.com/gsoultan/raorm/internal/planspike/store/org"
	"github.com/gsoultan/raorm/internal/planspike/store/user"
)

// THE GATE for greatest-n-per-group: each parent keeps its own N, and it is
// still two round trips. A loop over parents would also produce the right rows
// — the round-trip count is the whole claim.
func TestChildTop_IsPerParentAndTwoRoundTrips(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)

	const parents, top = 10, 3
	count.Reset()
	rows, err := store.OrgWithUsers().Limit(parents).
		ChildOrder(user.Email.Asc(), user.ID.Asc()).
		ChildTop(top).
		All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != parents {
		t.Fatalf("got %d parents, want %d", len(rows), parents)
	}
	if n := count.RoundTrips(); n != 2 {
		t.Errorf("%d round trips for %d parents, want exactly 2", n, parents)
	}

	for _, r := range rows {
		if len(r.Users) != top {
			t.Fatalf("org %s got %d users, want %d — the limit is not per parent",
				r.Name, len(r.Users), top)
		}
		for _, u := range r.Users {
			if u.OrgID != r.ID {
				t.Fatal("a child was attached to the wrong parent")
			}
		}
		for i := 1; i < len(r.Users); i++ {
			if r.Users[i-1].Email > r.Users[i].Email {
				t.Fatalf("org %s: children are not in the requested order", r.Name)
			}
		}
	}

	// It must be the FIRST n by the ordering, not an arbitrary n.
	full, err := user.New().Where(user.OrgID.Eq(rows[0].ID)).
		Order(user.Email.Asc(), user.ID.Asc()).Limit(top).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range full {
		if full[i].ID != rows[0].Users[i].ID {
			t.Errorf("child %d differs from the same query run alone — not the first %d by the ordering", i, top)
		}
	}
}

// Without an ordering, "the first three" is an arbitrary three and a different
// arbitrary three next call. Say so rather than returning one.
func TestChildTop_RequiresAnOrdering(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	_, err := store.OrgWithUsers().Limit(2).ChildTop(3).All(ctx, ex)
	if err == nil {
		t.Fatal("a per-parent limit without an ordering must be an error")
	}
	if !strings.Contains(err.Error(), "ordering") {
		t.Errorf("the error should say what is missing, got: %v", err)
	}
}

// The global guard must not fire on a per-parent load: it is bounded by
// construction, so tripping ErrChildLimit there would be a false alarm.
func TestChildTop_DoesNotTripTheGlobalGuard(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	rows, err := store.OrgWithUsers().Limit(20).
		ChildOrder(user.ID.Asc()).
		ChildTop(2).
		ChildLimit(5). // far below 20 parents x 2, and irrelevant here
		All(ctx, ex)
	if err != nil {
		t.Fatalf("a per-parent load is bounded by construction: %v", err)
	}
	if len(rows) != 20 {
		t.Fatalf("got %d parents, want 20", len(rows))
	}
}

// A self-referential hierarchy uses the same machinery.
func TestChildTop_WorksOnASelfReference(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	count.Reset()

	rows, err := store.OrgWithChildren().Limit(5).
		ChildOrder(org.Name.Asc(), org.ID.Asc()).
		ChildTop(2).
		All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("got %d orgs, want 5", len(rows))
	}
	if n := count.RoundTrips(); n != 2 {
		t.Errorf("%d round trips, want 2", n)
	}
}

// Both lowerings are generated so they can be compared on identical generated
// code. They must agree on the rows, or the benchmark is comparing two
// different queries.
func TestChildTop_BothLoweringsAgree(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	orgs, err := org.New().Limit(8).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([][16]byte, len(orgs))
	for i, o := range orgs {
		ids[i] = o.ID
	}
	order := []user.Sort{user.Email.Asc(), user.ID.Asc()}

	win, err := user.BatchTopByOrgIDWindow(ctx, ex, ids, 4, order...)
	if err != nil {
		t.Fatal(err)
	}
	lat, err := user.BatchTopByOrgIDLateral(ctx, ex, ids, 4, order...)
	if err != nil {
		t.Fatal(err)
	}
	if len(win) != len(lat) {
		t.Fatalf("window returned %d rows, lateral %d", len(win), len(lat))
	}
	seen := make(map[[16]byte]bool, len(win))
	for _, r := range win {
		seen[r.ID] = true
	}
	for _, r := range lat {
		if !seen[r.ID] {
			t.Fatal("the two lowerings returned different rows")
		}
	}
}
