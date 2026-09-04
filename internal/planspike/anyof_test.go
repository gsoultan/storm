package planspike_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gsoultan/storm/internal/planspike/store/user"
)

// AnyOf is the shape a filter screen produces and Any cannot say. Any ORs
// single predicates, so `(status = 'pending' AND age >= 30) OR (status =
// 'active' AND age < 20)` had no expression in the generated API at all and
// the query had to be written in SQL.
func TestAnyOf_ORsWholeConjunctions(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	cap := &capture{Executor: ex}

	_, err := user.New().
		AnyOf(
			user.And(user.Status.Eq("pending"), user.Age.Gte(30)),
			user.And(user.Status.Eq("active"), user.Age.Lt(20)),
		).
		All(ctx, cap, nil)
	if err != nil {
		t.Fatal(err)
	}

	// The outermost parentheses are unwrapped: a WHERE clause is already a
	// grouping, and `WHERE (x)` and `WHERE x` are the same statement.
	sql := cap.sqls[len(cap.sqls)-1]
	want := `WHERE ("status" = $1 AND "age" >= $2) OR ("status" = $3 AND "age" < $4)`
	if !strings.Contains(sql, want) {
		t.Fatalf("want\n  %s\nin\n  %s", want, sql)
	}
}

// The groups compose with everything else: a top-level predicate is ANDed with
// the disjunction, and the placeholders are numbered across both in one pass.
func TestAnyOf_ComposesWithWhere(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	cap := &capture{Executor: ex}

	_, err := user.New().
		Where(user.Active.Eq(true)).
		AnyOf(
			user.And(user.Status.Eq("pending"), user.Age.Gte(30)),
			user.And(user.Status.Eq("active"), user.Age.Lt(20)),
		).
		All(ctx, cap, nil)
	if err != nil {
		t.Fatal(err)
	}

	sql := cap.sqls[len(cap.sqls)-1]
	for _, want := range []string{
		`"active" = $1`,
		`(("status" = $2 AND "age" >= $3) OR ("status" = $4 AND "age" < $5))`,
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("want\n  %s\nin\n  %s", want, sql)
		}
	}
}

// A screen that builds one group per filled-in filter row needs no special
// case for the rows left blank, and a group of one predicate must not grow a
// pair of parentheses — the SQL, and so the shape it caches under, has to
// match the equivalent Where.
func TestAnyOf_DegenerateGroups(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	for _, c := range []struct {
		name string
		q    user.Query
		want string
	}{
		{"no groups at all", user.New().AnyOf(), ""},
		{"every group empty", user.New().AnyOf(user.And(), user.And()), ""},
		{"one group of one", user.New().AnyOf(user.And(user.Status.Eq("pending"))),
			`"status" = $1`},
		{"one group of two", user.New().AnyOf(user.And(user.Status.Eq("pending"), user.Active.Eq(true))),
			`"status" = $1 AND "active" = $2`},
		{"an empty group beside a real one", user.New().AnyOf(user.And(), user.And(user.Status.Eq("pending"))),
			`"status" = $1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			cap := &capture{Executor: ex}
			if _, err := c.q.All(ctx, cap, nil); err != nil {
				t.Fatal(err)
			}
			sql := cap.sqls[len(cap.sqls)-1]
			if c.want == "" {
				if strings.Contains(sql, "WHERE") {
					t.Fatalf("a query with no predicates grew a WHERE:\n%s", sql)
				}
				return
			}
			if !strings.Contains(sql, "WHERE "+c.want) {
				t.Fatalf("want WHERE %s in\n%s", c.want, sql)
			}
		})
	}
}

// NotAnyOf is the anti-filter: none of these groups matched.
func TestNotAnyOf_NegatesTheDisjunction(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	cap := &capture{Executor: ex}

	_, err := user.New().
		NotAnyOf(
			user.And(user.Status.Eq("pending"), user.Age.Gte(30)),
			user.And(user.Status.Eq("active")),
		).
		All(ctx, cap, nil)
	if err != nil {
		t.Fatal(err)
	}

	sql := cap.sqls[len(cap.sqls)-1]
	want := `NOT ((("status" = $1 AND "age" >= $2) OR "status" = $3))`
	if !strings.Contains(sql, want) {
		t.Fatalf("want\n  %s\nin\n  %s", want, sql)
	}
}

// NotAnyOf over nothing must not emit a NOT with no operand — the token stream
// would pop an entry that belongs to another predicate.
func TestNotAnyOf_OfNothingIsNoPredicate(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)
	cap := &capture{Executor: ex}

	if _, err := user.New().Where(user.Active.Eq(true)).NotAnyOf().All(ctx, cap, nil); err != nil {
		t.Fatal(err)
	}
	sql := cap.sqls[len(cap.sqls)-1]
	if strings.Contains(sql, "NOT") {
		t.Fatalf("an empty NotAnyOf negated something:\n%s", sql)
	}
	if !strings.Contains(sql, `WHERE "active" = $1`) {
		t.Fatalf("the other predicate was lost:\n%s", sql)
	}
}

// The whole thesis is that a warm dynamic query builds its SQL without
// allocating. A group is a slice, and a slice is the thing that usually
// escapes — so this is the assertion that decides whether AnyOf belongs in the
// generated API at all or is sugar with a hidden cost.
func TestAnyOf_IsZeroAlloc(t *testing.T) {
	if n := testing.AllocsPerRun(1000, func() {
		q := user.New().
			Where(user.Active.Eq(true)).
			AnyOf(
				user.And(user.Status.Eq("pending"), user.Age.Gte(30)),
				user.And(user.Status.Eq("active"), user.Age.Lt(20)),
			).
			Limit(20)
		if q.Err() != nil {
			t.Fatal(q.Err())
		}
	}); n != 0 {
		t.Errorf("composing a grouped disjunction allocates %v times, want 0", n)
	}
}

// The SQL being right is not the same as the ROWS being right: a disjunction
// whose operands are ANDed in the wrong order parses, plans and returns the
// wrong set. Asserted against the database, by comparing the one query with
// the two it replaces.
func TestAnyOf_ReturnsTheUnionOfItsGroups(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	// Both groups have to MATCH something, and the two-predicate shape is the
	// point: each group pairs a column every row shares with one that
	// separates them, so a lowering that ORed the wrong operands would return
	// every row rather than these.
	left := user.And(user.Status.Eq("pending"), user.Email.Like("u1@%"))
	right := user.And(user.Status.Eq("pending"), user.Email.Like("u2@%"))

	both, err := user.New().AnyOf(left, right).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := user.New().Where(user.Status.Eq("pending"), user.Email.Like("u1@%")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := user.New().Where(user.Status.Eq("pending"), user.Email.Like("u2@%")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) == 0 || len(b) == 0 {
		t.Fatalf("one group matches nothing (%d, %d); the union proves nothing", len(a), len(b))
	}
	// And the disjunction must be narrower than the table, or "returns the
	// union" would also be satisfied by returning everything.
	total, err := user.New().Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}

	want := map[[16]byte]bool{}
	for _, r := range a {
		want[r.ID] = true
	}
	for _, r := range b {
		want[r.ID] = true
	}
	if len(both) != len(want) {
		t.Fatalf("AnyOf returned %d rows; the two queries it replaces return %d distinct",
			len(both), len(want))
	}
	for _, r := range both {
		if !want[r.ID] {
			t.Fatalf("AnyOf returned a row neither group matches: %x", r.ID)
		}
	}
	if int64(len(both)) >= total {
		t.Fatalf("the disjunction returned %d of %d rows — it is not filtering",
			len(both), total)
	}
}
