package planspike_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/gsoultan/raorm"
	"github.com/gsoultan/raorm/compile/pgddl"
	"github.com/gsoultan/raorm/internal/planspike"
	"github.com/gsoultan/raorm/internal/planspike/store/org"
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

			rows, err := planspike.OrgsWithUsers().Limit(parents).All(ctx, ex)
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

	rows, err := planspike.OrgsWithUsers().Where(org.Name.Eq("no such org")).All(ctx, ex)
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

	rows, err := planspike.OrgsWithUsers().
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

	_, err := planspike.OrgsWithUsers().Limit(10).ChildLimit(100).All(ctx, ex)
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
