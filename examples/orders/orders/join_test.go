package orders_test

import (
	"context"
	"testing"

	"example.com/orders/orders"
	"example.com/orders/store/order"
)

// A join projects across tables and returns a flat row. It must agree with the
// SQL a human would write — same rows, same values.
func TestJoinWithCustomer(t *testing.T) {
	ctx := context.Background()
	svc := orders.New(pool)
	cust, _ := seed(t, "SKU-JOIN", "11.00", 50)
	if _, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-JOIN", Quantity: 2}}); err != nil {
		t.Fatal(err)
	}

	got, err := order.New().AllWithCustomer(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no joined rows")
	}

	var want int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM orders JOIN customers ON orders.customer_id = customers.id`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != want {
		t.Errorf("storm's join returned %d rows, hand-written SQL returns %d", len(got), want)
	}

	// Every row carries the customer's columns, not a zero value.
	for _, r := range got {
		if r.CustomerEmail == "" {
			t.Errorf("order %x joined a customer with no email", r.OrderID)
		}
	}

	// Declared ordering: newest first, and stable across calls.
	for i := 1; i < len(got); i++ {
		if got[i].PlacedAt.After(got[i-1].PlacedAt) {
			t.Fatalf("row %d is newer than row %d — the declared ORDER BY did not apply", i, i-1)
		}
	}
}

// Call-site predicates apply to the declaring table and compose with the join.
func TestJoinPredicatesCompose(t *testing.T) {
	ctx := context.Background()
	all, err := order.New().AllWithCustomer(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	narrowed, err := order.New().Where(order.Status.Eq("pending")).AllWithCustomer(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(narrowed) > len(all) {
		t.Errorf("a predicate widened the join: %d > %d", len(narrowed), len(all))
	}
	for _, r := range narrowed {
		if r.Status != "pending" {
			t.Errorf("the predicate let through status %q", r.Status)
		}
	}
}

// A CTE: the aggregate is computed once and joined against, instead of a
// correlated subquery per row.
func TestJoinWithCTE(t *testing.T) {
	ctx := context.Background()
	svc := orders.New(pool)
	cust, _ := seed(t, "SKU-CTE", "7.00", 50)
	for i := 0; i < 3; i++ {
		if _, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-CTE", Quantity: 1}}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := order.New().AllVsLifetime(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no rows")
	}

	// Every order of a customer carries that customer's LIFETIME totals, which
	// is the whole point: the per-row Total differs, the Lifetime does not.
	byEmail := map[string]int64{}
	for _, r := range got {
		n, ok := r.LifetimeOrders.Get()
		if !ok {
			t.Errorf("%s has no lifetime row; the LEFT join found nothing", r.CustomerEmail)
			continue
		}
		if prev, seen := byEmail[r.CustomerEmail]; seen && prev != n {
			t.Errorf("%s reports %d lifetime orders on one row and %d on another",
				r.CustomerEmail, prev, n)
		}
		byEmail[r.CustomerEmail] = n
		if _, ok := r.Lifetime.Get(); !ok {
			t.Errorf("%s has a lifetime order count but no lifetime spend", r.CustomerEmail)
		}
	}

	// And it must agree with the same CTE written by hand.
	rows, err := pool.Query(ctx, `
		WITH spend AS (SELECT customer_id, count(*) n FROM orders GROUP BY customer_id)
		SELECT c.email, s.n
		  FROM orders o JOIN customers c ON o.customer_id = c.id
		  LEFT JOIN spend s ON s.customer_id = c.id
		 GROUP BY c.email, s.n`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var email string
		var n int64
		if err := rows.Scan(&email, &n); err != nil {
			t.Fatal(err)
		}
		if got, ok := byEmail[email]; ok && got != n {
			t.Errorf("%s: storm says %d lifetime orders, hand-written SQL says %d", email, got, n)
		}
	}
}

// Order() on a join is a category error — a bare column name in a multi-table
// result is ambiguous — so it is refused by name rather than silently ignored.
func TestOrderOnJoinIsRefused(t *testing.T) {
	ctx := context.Background()
	_, err := order.New().Order(order.PlacedAt.Desc()).AllWithCustomer(ctx, ex)
	if err == nil {
		t.Fatal("Order() on a join was accepted and silently ignored")
	}
	if !contains(err.Error(), "join") {
		t.Errorf("error does not explain itself: %v", err)
	}
}

// A declared Where cannot be widened by a call site. The declaration says
// cancelled orders are not part of this read; a caller asking for them gets
// nothing, rather than getting them.
func TestJoinDeclaredWhereCannotBeWidened(t *testing.T) {
	ctx := context.Background()
	// Seeds its own row rather than depending on another test's: the suite
	// runs with -shuffle=on, so "some order exists" is not a fact here.
	svc := orders.New(pool)
	cust, _ := seed(t, "SKU-CANCEL", "3.00", 20)
	placed, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-CANCEL", Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE orders SET status = 'cancelled' WHERE id = $1`,
		parseUUID(t, placed.OrderID)); err != nil {
		t.Fatal(err)
	}

	// Asking FOR cancelled orders must still return none: the declared
	// predicate is ANDed, not replaced.
	got, err := order.New().Where(order.Status.Eq("cancelled")).AllWithCustomer(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a call site widened a declared Where: got %d cancelled rows", len(got))
	}

	// And the unfiltered read excludes them too.
	all, err := order.New().AllWithCustomer(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if r.Status == "cancelled" {
			t.Error("the declared Where did not apply to an unfiltered read")
		}
	}
}
