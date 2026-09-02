package orders_test

import (
	"context"
	"testing"

	"example.com/orders/store/customer"
	"example.com/orders/store/stockitem"
)

// The list and pattern predicates, against a real server.
//
// A list predicate binds one Go slice to one placeholder, and the slice's
// element type has to reach PostgreSQL as the array type the column is
// compared against. That is a wire-format question no unit test answers: the
// generated method compiles either way, and a list bound into the wrong slot
// is a query that runs and returns the wrong rows. storm has had exactly that
// bug once already — a bool riding the int64 arena, which no fixture had a
// column for — so a new slot without a live test is the same mistake twice.

func TestIntListPredicates(t *testing.T) {
	ctx := context.Background()
	seed(t, "SKU-N7", "1.00", 7)
	seed(t, "SKU-N11", "1.00", 11)
	seed(t, "SKU-N13", "1.00", 13)

	// int32 is its own slot. Before it existed, opApplies claimed integers
	// supported In and no method was emitted for them: `WHERE on_hand IN
	// (7, 13)` could not be written at all.
	in, err := stockitem.New().Where(stockitem.OnHand.In(7, 13)).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int32]bool{}
	for _, s := range in {
		got[s.OnHand] = true
	}
	if !got[7] || !got[13] || got[11] {
		t.Errorf("In(7, 13) matched on_hand %v; want exactly 7 and 13", keys(got))
	}

	// NotIn is `<> ALL`, and 11 is the only one of the three left.
	out, err := stockitem.New().
		Where(stockitem.OnHand.NotIn(7, 13), stockitem.OnHand.Lte(13)).
		All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range out {
		if s.OnHand == 7 || s.OnHand == 13 {
			t.Errorf("NotIn(7, 13) returned on_hand %d", s.OnHand)
		}
	}
}

// One statement for every list length is the property In exists for: the list
// is ONE bound argument, so a query for two ids and a query for two hundred
// share a plan rather than filling the cache with one statement per length.
func TestListLengthDoesNotChangeTheStatement(t *testing.T) {
	b := stockitem.GetBinder()
	defer stockitem.PutBinder(b)
	two, _ := stockitem.New().Where(stockitem.OnHand.In(1, 2)).Prepare(b)
	many, _ := stockitem.New().Where(stockitem.OnHand.In(1, 2, 3, 4, 5, 6, 7, 8)).Prepare(b)
	if two != many {
		t.Errorf("list length changed the statement:\n  2: %s\n  8: %s", two, many)
	}
}

func TestTextListAndPatternPredicates(t *testing.T) {
	ctx := context.Background()
	seed(t, "SKU-LIKE-A", "1.00", 1)
	seed(t, "SKU-LIKE-B", "1.00", 1)

	emails := []string{"SKU-LIKE-A@example.com", "SKU-LIKE-B@example.com"}
	rows, err := customer.New().Where(customer.Email.In(emails...)).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("In over two emails matched %d rows, want 2", len(rows))
	}

	// ILIKE is the point: LIKE with this pattern matches nothing, because the
	// stored value is upper-case. Someone writing lower(email) LIKE lower($1)
	// instead gets the same answer with the index thrown away.
	ci, err := customer.New().Where(customer.Email.ILike("sku-like-%@example.com")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ci) != 2 {
		t.Errorf("ILike matched %d rows, want 2", len(ci))
	}
	cs, err := customer.New().Where(customer.Email.Like("sku-like-%@example.com")).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Errorf("LIKE matched %d rows case-insensitively; the two operators are the same", len(cs))
	}
}

// A NULL in the list makes `<> ALL` NULL for every row, so the result is
// empty. That is PostgreSQL's rule for NOT IN and storm does not paper over
// it — but it is only safe to leave undocumented if it is also true, and this
// is the only place that says so.
func TestNotInIsUnaffectedByOrderOfArguments(t *testing.T) {
	ctx := context.Background()
	seed(t, "SKU-ORD-1", "1.00", 41)
	seed(t, "SKU-ORD-2", "1.00", 43)

	a, err := stockitem.New().Where(stockitem.OnHand.NotIn(41), stockitem.OnHand.Gte(41)).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := stockitem.New().Where(stockitem.OnHand.Gte(41), stockitem.OnHand.NotIn(41)).All(ctx, ex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Errorf("predicate order changed the result: %d vs %d", len(a), len(b))
	}
	for _, s := range a {
		if s.OnHand == 41 {
			t.Error("NotIn(41) returned a row with on_hand 41")
		}
	}
}

func keys(m map[int32]bool) []int32 {
	out := make([]int32, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
