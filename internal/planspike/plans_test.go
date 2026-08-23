package planspike_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/gsoultan/raorm"
	"github.com/gsoultan/raorm/compile/pgddl"
	"github.com/gsoultan/raorm/internal/planspike/store"
	"github.com/gsoultan/raorm/internal/planspike/store/org"
	"github.com/gsoultan/raorm/internal/planspike/store/user"
	"github.com/gsoultan/raorm/internal/testmodel"
	"github.com/gsoultan/raorm/runtime"
	"github.com/gsoultan/raorm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	nOrgs         = 50
	usersPerOrg   = 500
	planspikeSchm = "planspike"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	dsn := os.Getenv("RAORM_DSN")
	if dsn == "" {
		fmt.Println("RAORM_DSN unset; skipping the plan spike")
		os.Exit(0)
	}
	ctx := context.Background()
	var err error
	cfg, err := pgxpool.ParseConfig(dsn)
	must(err)
	// search_path must be set on the *pool*, not on one checked-out connection.
	// Setting it with a SET statement configures whichever connection happened
	// to serve it, so a concurrent test lands on a different one and sees the
	// public schema — which cost a debugging round.
	cfg.ConnConfig.RuntimeParams["search_path"] = planspikeSchm
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	defer pool.Close()

	// Its own namespace, so the spike cannot collide with the migrate tests
	// running shuffled alongside it.
	must(run(ctx, "DROP SCHEMA IF EXISTS "+planspikeSchm+" CASCADE"))
	must(run(ctx, "CREATE SCHEMA "+planspikeSchm))

	s, err := raorm.Build(testmodel.All()...)
	must(err)
	must(run(ctx, pgddl.Create(s)))
	must(seed(ctx))

	os.Exit(m.Run())
}

func run(ctx context.Context, sql string) error {
	_, err := pool.Exec(ctx, sql)
	return err
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func seed(ctx context.Context) error {
	return run(ctx, fmt.Sprintf(`
		INSERT INTO orgs (id, created_at, updated_at, name)
		SELECT gen_random_uuid(), now(), now(), 'org-'||g
		FROM generate_series(1, %d) g;

		INSERT INTO users (id, created_at, updated_at, deleted_at, email, name,
		                   status, prefs, scopes, age, last_ip, org_id)
		SELECT gen_random_uuid(), now(), now(), NULL,
		       'u'||n||'@'||o.name, 'user '||n, 'pending', '{}'::jsonb,
		       ARRAY[]::text[], NULL, NULL, o.id
		FROM orgs o, generate_series(1, %d) n;
	`, nOrgs, usersPerOrg))
}

func db(t *testing.T) (runtime.Executor, *runtime.CountingExecutor) {
	t.Helper()
	c := &runtime.CountingExecutor{Inner: pgxdrv.Pool{P: pool}}
	return c, c
}

// THE GATE. Fifty parents and twenty-five thousand children in exactly two
// round trips — and it must hold as the parent count changes, because a loader
// that batches per parent still passes at n=1.
func TestPlan_TwoRoundTrips(t *testing.T) {
	ctx := context.Background()
	for _, parents := range []int64{1, 7, nOrgs} {
		t.Run(fmt.Sprint(parents), func(t *testing.T) {
			ex, count := db(t)
			count.Reset()

			rows, err := store.OrgWithUsers().Limit(parents).All(ctx, ex)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(rows)) != parents {
				t.Fatalf("got %d orgs, want %d", len(rows), parents)
			}
			if n := count.RoundTrips(); n != 2 {
				t.Errorf("%d round trips, want exactly 2 — the = ANY batch is the N+1 guarantee", n)
			}
			for _, r := range rows {
				if len(r.Users) != usersPerOrg {
					t.Fatalf("org %s got %d users, want %d", r.Name, len(r.Users), usersPerOrg)
				}
			}
		})
	}
}

// An empty parent set must not issue a guaranteed-empty child query.
func TestPlan_NoParentsIsOneRoundTrip(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	count.Reset()

	rows, err := store.OrgWithUsers().Where(org.Name.Eq("no such org")).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
	if n := count.RoundTrips(); n != 1 {
		t.Errorf("%d round trips, want 1", n)
	}
}

// Predicates on the parent must compose exactly as they do on a bare query —
// a plan that only accepts a subset of the builder is a plan people route
// around.
func TestPlan_ParentPredicatesCompose(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	count.Reset()

	rows, err := store.OrgWithUsers().
		Where(org.Name.Gte("org-1"), org.Name.Lte("org-2")).
		WhereIf(false, org.Name.Eq("ignored")).
		Limit(5).
		All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("expected some orgs in range")
	}
	for _, r := range rows {
		if r.Name < "org-1" || r.Name > "org-2" {
			t.Errorf("org %q is outside the predicate range", r.Name)
		}
	}
	if n := count.RoundTrips(); n != 2 {
		t.Errorf("%d round trips, want 2", n)
	}
}

// A partial relation load is worse than a failed one: every count computed
// from it is wrong and nothing says so.
func TestPlan_ChildLimitIsAnErrorNotATruncation(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	_, err := store.OrgWithUsers().Limit(10).ChildLimit(100).All(ctx, ex)
	if err == nil {
		t.Fatal("hitting the child limit must be an error, not a silently partial result")
	}
}

// orgRows is the ids of seeded orgs, for tests that need a valid foreign key.
func orgRows(ctx context.Context, ex runtime.Executor) ([][16]byte, error) {
	rows, err := org.New().Limit(1).All(ctx, ex, nil)
	if err != nil {
		return nil, err
	}
	out := make([][16]byte, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out, nil
}

// All four relation kinds the fixture declares, each in exactly two round
// trips. The kinds differ in where the key lives and whether it is nullable,
// which is precisely where a loader gets them wrong.
func TestPlan_EveryRelationKindIsTwoRoundTrips(t *testing.T) {
	ctx := context.Background()

	t.Run("has-many", func(t *testing.T) {
		ex, count := db(t)
		count.Reset()
		rows, err := store.OrgWithUsers().Limit(5).All(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 5 {
			t.Fatalf("got %d orgs, want 5", len(rows))
		}
		for _, r := range rows {
			if len(r.Users) != usersPerOrg {
				t.Fatalf("org %s has %d users, want %d", r.Name, len(r.Users), usersPerOrg)
			}
		}
		if n := count.RoundTrips(); n != 2 {
			t.Errorf("%d round trips, want 2", n)
		}
	})

	t.Run("belongs-to", func(t *testing.T) {
		ex, count := db(t)
		count.Reset()
		// A thousand users pointing at fifty orgs must fetch fifty orgs, not a
		// thousand: distinct keys are de-duplicated before the second query.
		rows, err := store.UserWithOrg().Limit(1000).All(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1000 {
			t.Fatalf("got %d users, want 1000", len(rows))
		}
		for _, r := range rows {
			if r.Org.ID != r.OrgID {
				t.Fatal("a user is joined to the wrong org")
			}
		}
		if n := count.RoundTrips(); n != 2 {
			t.Errorf("%d round trips, want 2", n)
		}
	})

	t.Run("self-referential has-many", func(t *testing.T) {
		ex, count := db(t)
		count.Reset()
		// orgs.parent_id is nullable — the root has nowhere to point — so this
		// is the case where the relation's Go field and its column disagree
		// about nullability.
		rows, err := store.OrgWithChildren().Limit(5).All(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 5 {
			t.Fatalf("got %d orgs, want 5", len(rows))
		}
		if n := count.RoundTrips(); n != 2 {
			t.Errorf("%d round trips, want 2", n)
		}
	})

	t.Run("self-referential to-one, all keys null", func(t *testing.T) {
		ex, count := db(t)
		count.Reset()
		// Every seeded org is a root, so no parent key is bound at all and the
		// second query must not be issued.
		rows, err := store.OrgWithParent().Limit(5).All(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 5 {
			t.Fatalf("got %d orgs, want 5", len(rows))
		}
		for _, r := range rows {
			if r.Parent != nil {
				t.Error("a seeded org has a parent it should not have")
			}
		}
		if n := count.RoundTrips(); n != 1 {
			t.Errorf("%d round trips, want 1 — no parent keys means nothing to fetch", n)
		}
	})
}

// The generated plan must reject a partial load, exactly as the hand-written
// spike did.
func TestPlan_GeneratedChildLimitIsAnError(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	if _, err := store.OrgWithUsers().Limit(10).ChildLimit(100).All(ctx, ex); !errors.Is(err, runtime.ErrChildLimit) {
		t.Errorf("hitting the child limit returned %v, want ErrChildLimit", err)
	}
}

// A plan must page its parents. Order, Offset and After exist on the table
// Query and were missing from every generated plan, so a plan could filter but
// not page — which is useless for the case plans exist for.
func TestPlan_PagesItsParents(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	page1, err := store.OrgWithUsers().Order(org.Name.Asc()).Limit(3).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 3 {
		t.Fatalf("page one has %d orgs, want 3", len(page1))
	}
	page2, err := store.OrgWithUsers().Order(org.Name.Asc()).
		After(page1[len(page1)-1]).Limit(3).All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) == 0 {
		t.Fatal("no second page of parents")
	}
	for _, a := range page1 {
		for _, b := range page2 {
			if a.ID == b.ID {
				t.Fatal("page two repeats a parent from page one")
			}
		}
	}
	if page2[0].Name <= page1[len(page1)-1].Name {
		t.Errorf("page two starts at %q, not after page one's last %q", page2[0].Name, page1[len(page1)-1].Name)
	}
	// The children must still be loaded on a paged plan.
	for _, r := range page2 {
		if len(r.Users) != usersPerOrg {
			t.Fatalf("paged parent %s has %d users, want %d", r.Name, len(r.Users), usersPerOrg)
		}
	}
}

// Children arrive in a caller-chosen order, not the child table's primary key.
func TestPlan_OrdersChildrenWithinEachParent(t *testing.T) {
	ctx := context.Background()
	ex, count := db(t)
	count.Reset()

	rows, err := store.OrgWithUsers().Limit(3).
		ChildOrder(user.Email.Desc()).
		All(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rows {
		for i := 1; i < len(r.Users); i++ {
			if r.Users[i-1].Email < r.Users[i].Email {
				t.Fatalf("org %s: children are not in descending email order at %d", r.Name, i)
			}
		}
	}
	if n := count.RoundTrips(); n != 2 {
		t.Errorf("%d round trips, want 2 — ordering children must not cost a query", n)
	}
}

// ChildLimit is a guard, not a page size, and the docs say so. This pins the
// behaviour so nobody "fixes" it into a silent per-parent truncation.
func TestPlan_ChildLimitIsGlobalNotPerParent(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	// 3 parents x usersPerOrg children, with a limit above one parent's worth
	// but below three parents' worth: a per-parent limit would succeed, a
	// global guard must refuse.
	_, err := store.OrgWithUsers().Limit(3).ChildLimit(int64(usersPerOrg)+1).All(ctx, ex)
	if err == nil {
		t.Fatal("ChildLimit is a global guard — three parents' children must not fit under one parent's limit")
	}
}
