package planspike_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gsoultan/raorm/internal/planspike/store/user"
)

// Ordering is chosen per query now, so the rows must actually come back in it.
func TestOrder_ChangesTheRowOrder(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	asc, err := user.New().Order(user.Email.Asc()).Limit(20).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := user.New().Order(user.Email.Desc()).Limit(20).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(asc) == 0 || len(desc) == 0 {
		t.Fatal("no rows")
	}
	for i := 1; i < len(asc); i++ {
		if asc[i-1].Email > asc[i].Email {
			t.Fatalf("ASC is not ascending at %d: %q then %q", i, asc[i-1].Email, asc[i].Email)
		}
	}
	for i := 1; i < len(desc); i++ {
		if desc[i-1].Email < desc[i].Email {
			t.Fatalf("DESC is not descending at %d: %q then %q", i, desc[i-1].Email, desc[i].Email)
		}
	}
	if asc[0].Email == desc[0].Email {
		t.Error("ASC and DESC returned the same first row")
	}
}

// An ordering is part of a statement's identity. If it were not, two queries
// differing only in ORDER BY would share a compiled statement and one of them
// would silently get the other's ordering.
func TestOrder_IsPartOfTheStatementKey(t *testing.T) {
	a := user.New().Order(user.Email.Asc())
	b := user.New().Order(user.Email.Desc())
	if a.Shape() == b.Shape() {
		t.Error("ASC and DESC share a shape — they would share a compiled statement")
	}
	if a.SQL() == b.SQL() {
		t.Errorf("ASC and DESC compiled to the same SQL:\n%s", a.SQL())
	}

	// Same ordering, different values: one statement, as ever.
	c := user.New().Where(user.Email.Eq("x")).Order(user.Email.Asc())
	d := user.New().Where(user.Email.Eq("y")).Order(user.Email.Asc())
	if c.Shape() != d.Shape() {
		t.Error("two queries differing only in a bound value have different shapes")
	}
}

// A read with no ordering has no defined order, so paging it is a bug waiting
// for a plan change. There is always an ORDER BY.
func TestOrder_DefaultIsNeverAbsent(t *testing.T) {
	if sql := user.New().SQL(); !strings.Contains(sql, "ORDER BY") {
		t.Errorf("a default query has no ORDER BY:\n%s", sql)
	}
}

// Composing an ordering must not allocate, for the same reason composing a
// predicate must not.
func TestOrder_IsZeroAlloc(t *testing.T) {
	if n := testing.AllocsPerRun(1000, func() {
		q := user.New().
			Where(user.Status.Eq("pending")).
			Order(user.Email.Asc(), user.CreatedAt.Desc()).
			Offset(10).
			Limit(20)
		if q.Err() != nil {
			t.Fatal(q.Err())
		}
	}); n != 0 {
		t.Errorf("composing an ordered, paged query allocates %v times, want 0", n)
	}
}

// OFFSET is a different statement from LIMIT alone: a different placeholder
// count, so it cannot share a compiled statement.
func TestOffset_IsADifferentStatement(t *testing.T) {
	plain := user.New().SQL()
	paged := user.New().Offset(10).SQL()
	if plain == paged {
		t.Error("OFFSET did not change the statement")
	}
	if !strings.Contains(paged, "LIMIT $1 OFFSET $2") {
		t.Errorf("placeholders are misnumbered:\n%s", paged)
	}
}

func TestOffset_SkipsRows(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	first, err := user.New().Order(user.Email.Asc()).Limit(10).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := user.New().Order(user.Email.Asc()).Offset(5).Limit(10).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 10 || len(rest) < 5 {
		t.Fatal("not enough rows to test paging")
	}
	if first[5].Email != rest[0].Email {
		t.Errorf("OFFSET 5 started at %q, want %q", rest[0].Email, first[5].Email)
	}
}

// Counting ignores ordering as well as LIMIT: ordering a scalar is wasted work,
// and it must not mint a second compiled statement per ordering.
func TestCount_IgnoresOrdering(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	a, err := user.New().Order(user.Email.Asc()).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	b, err := user.New().Order(user.CreatedAt.Desc()).Count(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("ordering changed a count: %d vs %d", a, b)
	}
}
