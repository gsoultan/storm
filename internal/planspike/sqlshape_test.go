package planspike_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/raorm/internal/planspike/store"
	"github.com/gsoultan/raorm/internal/planspike/store/org"
	"github.com/gsoultan/raorm/internal/planspike/store/post"
	"github.com/gsoultan/raorm/internal/planspike/store/user"
	"github.com/gsoultan/raorm/runtime"
)

// capture records every statement an executor issues, so a test can assert on
// the SQL a path ACTUALLY runs rather than the SQL a doc says it runs.
type capture struct {
	runtime.Executor
	sqls []string
}

func (c *capture) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	c.sqls = append(c.sqls, sql)
	return c.Executor.Query(ctx, sql, args)
}

// Exists asks a one-bit question and the statement must match: SELECT 1, no
// ORDER BY, LIMIT 1. The old shape projected every column under the default
// ordering — the planner top-N-sorted 16,667 matching rows to return one.
func TestExists_IsAOneBitStatement(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	cap := &capture{Executor: ex}

	ok, err := user.New().Where(user.Status.Eq("pending")).Exists(ctx, cap)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("seeded users exist")
	}
	sql := cap.sqls[len(cap.sqls)-1]
	if !strings.HasPrefix(sql, `SELECT 1 FROM "users"`) {
		t.Errorf("Exists projects more than the question needs:\n%s", sql)
	}
	if strings.Contains(sql, "ORDER BY") {
		t.Errorf("Exists carries an ordering nobody reads:\n%s", sql)
	}
	if !strings.Contains(sql, "LIMIT 1") {
		t.Errorf("Exists does not stop at the first match:\n%s", sql)
	}

	none, err := user.New().Where(user.Email.Eq("nobody@nowhere.invalid")).Exists(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if none {
		t.Error("Exists found a row that does not exist")
	}
}

// Count with an Offset set used to bind mismatched arguments: bind appended
// [preds..., limit, offset] and Count sliced ONE argument off the end —
// dropping the offset and keeping the limit its statement has no placeholder
// for. Latent until someone composed the two.
func TestCount_IgnoresOffsetWithoutBreaking(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	plain, err := user.New().Where(user.Status.Eq("pending")).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	paged, err := user.New().Where(user.Status.Eq("pending")).Offset(5).Limit(3).Count(ctx, ex)
	if err != nil {
		t.Fatalf("Count composed with Offset: %v", err)
	}
	if plain != paged {
		t.Errorf("Count = %d with paging set, %d without — a count ignores paging", paged, plain)
	}
}

// Unordered drops the default ordering and only the default: an explicit
// Order still applies.
func TestUnordered_DropsOnlyTheDefault(t *testing.T) {
	if sql := user.New().Unordered().SQL(); strings.Contains(sql, "ORDER BY") {
		t.Errorf("Unordered still ordered:\n%s", sql)
	}
	if sql := user.New().Unordered().Order(user.Email.Asc()).SQL(); !strings.Contains(sql, "ORDER BY") {
		t.Errorf("an explicit Order was dropped:\n%s", sql)
	}
	// Ordered and unordered are different statements sharing nothing.
	if user.New().SQL() == user.New().Unordered().SQL() {
		t.Error("ordered and unordered compiled to the same statement")
	}
}

// A keyset cursor without an ordering is a position in nothing.
func TestUnordered_AfterIsAnError(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	q := user.New().Unordered().After(user.Row{})
	if q.Err() == nil {
		t.Fatal("After on an unordered query must be an error")
	}
	if _, err := q.All(ctx, ex, nil); err == nil {
		t.Error("the terminal must return it too")
	} else if !strings.Contains(err.Error(), "position in nothing") {
		t.Errorf("the error should say why, got: %v", err)
	}
	_ = errors.Is
}

// The relation loaders bucket rows into maps, so their child statements must
// not pay for an order — measured at 50k rows: an external merge sort spilling
// 5MB to disk, destroyed on arrival. ChildOrder still applies when asked for.
func TestPlanLoaders_DoNotOrderWhatTheyBucket(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	cap := &capture{Executor: ex}
	if _, err := store.OrgWithUsers().Limit(3).All(ctx, cap); err != nil {
		t.Fatal(err)
	}
	if len(cap.sqls) != 2 {
		t.Fatalf("expected 2 statements, saw %d", len(cap.sqls))
	}
	if strings.Contains(cap.sqls[1], "ORDER BY") {
		t.Errorf("the child batch pays for an order the map bucketing destroys:\n%s", cap.sqls[1])
	}

	cap = &capture{Executor: ex}
	if _, err := store.OrgWithUsers().Limit(3).ChildOrder(user.Email.Desc()).All(ctx, cap); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cap.sqls[1], "ORDER BY") {
		t.Errorf("an explicit ChildOrder was dropped:\n%s", cap.sqls[1])
	}
}

// HasPosts is the semi-join: EXISTS over the relation's key, the fastest way
// to ask "has any related row" — one probe per parent row, no join
// duplication, no DISTINCT to clean it up. It composes under Where/Any/Not
// like any predicate because the fragment is constant.
func TestHasRelation_IsASemiJoin(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	sql := user.New().Where(user.HasPosts()).SQL()
	if !strings.Contains(sql, `EXISTS (SELECT 1 FROM "posts" AS "_raorm_e"`) {
		t.Errorf("HasPosts did not lower to a semi-join:\n%s", sql)
	}

	// Live: the seeded users have no posts; give one a post and the partition
	// must be exact from both sides.
	total, err := user.New().Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	author := mustCreate(t, ctx, ex, "author@example.com", "Author")
	np := post.Create()
	np.SetTitle("t")
	np.SetBody("b")
	np.SetAuthorID(author.ID)
	pr, err := np.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = post.Delete(ctx, ex, pr.ID)
		_ = user.Delete(ctx, ex, author.ID)
	})

	with, err := user.New().Where(user.HasPosts()).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	without, err := user.New().Where(user.HasNoPosts()).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if with != 1 {
		t.Errorf("HasPosts matched %d users, want 1", with)
	}
	if with+without != total+1 {
		t.Errorf("partition leaks: %d with + %d without != %d total", with, without, total+1)
	}
}

// A self-referential existence check correlates a table with itself, which is
// exactly where an unaliased inner reference would capture the outer table and
// silently mean something else. The alias makes it correct; the test proves
// it against real parent/child rows.
func TestHasRelation_SelfReferenceIsAliased(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	sql := org.New().Where(org.HasChildren()).SQL()
	if !strings.Contains(sql, `"_raorm_e"."parent_id" = "orgs"."id"`) {
		t.Errorf("the self-referential correlation is not aliased:\n%s", sql)
	}

	parent := tree(t, ctx, ex, "semi", 2) // parent → child chain
	n, err := org.New().Where(org.ID.In(parent...), org.HasChildren()).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("of a two-org chain, %d have children; want exactly the parent", n)
	}
}

// Existence predicates are ordinary predicates: they compose under Any/Not,
// share compiled statements by structure, and cost nothing to build.
func TestHasRelation_ComposesAndCostsNothing(t *testing.T) {
	// The fixture's inverse one-to-one (Profile) never reaches the IR's
	// Relations — a known gap — so composition is shown over the two
	// polarities, which are distinct predicates in the frag table.
	a := user.New().Where(user.Status.Eq("pending")).Any(user.HasPosts(), user.HasNoPosts())
	if sql := a.SQL(); !strings.Contains(sql, " OR ") || strings.Count(sql, "EXISTS") != 2 {
		t.Errorf("composition is off:\n%s", sql)
	}
	if user.New().Where(user.HasPosts()).Shape() == user.New().Where(user.HasNoPosts()).Shape() {
		t.Error("Has and HasNo share a shape — they would share a statement")
	}
	if n := testing.AllocsPerRun(1000, func() {
		q := user.New().Where(user.Status.Eq("x"), user.HasPosts()).Limit(10)
		if q.Err() != nil {
			t.Fatal(q.Err())
		}
	}); n != 0 {
		t.Errorf("a semi-join predicate allocates %v times per build, want 0", n)
	}
}
