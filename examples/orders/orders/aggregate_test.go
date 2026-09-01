package orders_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/runtime"

	"example.com/orders/orders"
	"example.com/orders/store/customer"
	"example.com/orders/store/order"
)

// The aggregation must agree with the SQL a human would write. Not "returns
// something plausible" — the same numbers, from the same server, in the same
// transaction-visible state.
func TestAggregateMatchesHandWrittenSQL(t *testing.T) {
	ctx := context.Background()
	svc := orders.New(pool)
	cust, _ := seed(t, "SKU-AGG", "12.50", 100)
	for i := 0; i < 5; i++ {
		if _, err := svc.PlaceOrder(ctx, cust,
			[]orders.LineRequest{{SKU: "SKU-AGG", Quantity: int32(i + 1)}}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := order.New().AllByStatus(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no groups")
	}

	// The same question, asked directly.
	type want struct {
		orders  int64
		revenue string
	}
	wants := map[string]want{}
	rows, err := pool.Query(ctx, `SELECT status::text, count(*), coalesce(sum(total),0)::text
	                                FROM orders GROUP BY status`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s, rev string
		var n int64
		if err := rows.Scan(&s, &n, &rev); err != nil {
			t.Fatal(err)
		}
		wants[s] = want{n, rev}
	}
	rows.Close()

	for _, g := range got {
		w, ok := wants[g.Status]
		if !ok {
			t.Errorf("storm produced a group %q the database does not have", g.Status)
			continue
		}
		if g.Orders != w.orders {
			t.Errorf("%s: count = %d, want %d", g.Status, g.Orders, w.orders)
		}
		rev, valid := g.Revenue.Get()
		if !valid {
			t.Errorf("%s: revenue is NULL over %d rows", g.Status, g.Orders)
			continue
		}
		if rev.String() != w.revenue {
			t.Errorf("%s: revenue = %s, want %s", g.Status, rev.String(), w.revenue)
		}
	}
	if len(got) != len(wants) {
		t.Errorf("storm returned %d groups, the database has %d", len(got), len(wants))
	}
}

// The case that turns a wrong type into a wrong ANSWER. sum/avg/min/max over
// zero rows are NULL; count is 0. If the generated field were a plain int64 or
// Decimal, NULL would decode as zero and "no orders" would be indistinguishable
// from "orders totalling nothing".
func TestAggregateOverZeroRowsIsNullNotZero(t *testing.T) {
	ctx := context.Background()
	// A valid status that no order has: the group is empty, not invalid.
	got, err := order.New().Where(order.Status.Eq("cancelled")).AllTotals(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("an ungrouped aggregate returned %d rows, want exactly 1", len(got))
	}
	if got[0].Orders != 0 {
		t.Errorf("count over zero rows = %d, want 0", got[0].Orders)
	}
	if _, valid := got[0].Revenue.Get(); valid {
		t.Error("sum over zero rows reported a value; it must be NULL, or " +
			"\"no rows\" becomes \"sums to zero\"")
	}
}

// Predicates filter the rows that go INTO the groups — a WHERE, not a HAVING.
func TestAggregatePredicatesCompose(t *testing.T) {
	ctx := context.Background()
	svc := orders.New(pool)
	cust, _ := seed(t, "SKU-COMPOSE", "1.00", 50)
	if _, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-COMPOSE", Quantity: 3}}); err != nil {
		t.Fatal(err)
	}

	all, err := order.New().AllTotals(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	narrowed, err := order.New().
		Where(order.Status.Eq("pending"), order.Total.Gt(mustDec(t, "2.00"))).
		AllTotals(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if narrowed[0].Orders > all[0].Orders {
		t.Errorf("a predicate widened the result: %d > %d", narrowed[0].Orders, all[0].Orders)
	}
	if narrowed[0].Orders == 0 {
		t.Error("the predicate matched nothing; the test proves nothing")
	}
}

// Order() on an aggregation is a category error: its rows are groups. It has
// to be refused, and refused by NAME, rather than silently ignored.
func TestOrderOnAggregateIsRefused(t *testing.T) {
	ctx := context.Background()
	_, err := order.New().Order(order.PlacedAt.Desc()).AllByStatus(ctx, ex)
	if err == nil {
		t.Fatal("Order() on an aggregation was accepted and silently ignored")
	}
	for _, want := range []string{"aggregation", "grouping columns"} {
		if !contains(err.Error(), want) {
			t.Errorf("error does not explain itself: %v", err)
		}
	}
}

// The grouped read is ordered, so two calls return the same order. PostgreSQL
// promises nothing here without ORDER BY.
func TestAggregateOrderIsStable(t *testing.T) {
	ctx := context.Background()
	first, err := order.New().AllByStatus(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := order.New().AllByStatus(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(first) {
			t.Fatalf("run %d returned %d groups, first returned %d", i, len(again), len(first))
		}
		for j := range again {
			if again[j].Status != first[j].Status {
				t.Fatalf("run %d group %d is %q, first run had %q — the result is unordered",
					i, j, again[j].Status, first[j].Status)
			}
		}
	}
}

// The whole point of the type table: an average of money is exact.
func TestAggregateMoneyIsExact(t *testing.T) {
	ctx := context.Background()
	svc := orders.New(pool)
	cust, _ := seed(t, "SKU-EXACT", "0.10", 100)
	for i := 0; i < 3; i++ {
		if _, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-EXACT", Quantity: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := order.New().Where(order.Total.Eq(mustDec(t, "0.10"))).AllTotals(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	rev, valid := got[0].Revenue.Get()
	if !valid {
		t.Fatal("revenue is NULL")
	}
	// 3 × 0.10 = 0.30 exactly. A float64 cannot represent 0.10 at all.
	if rev.String() != "0.30" {
		t.Errorf("sum of three 0.10 orders = %s, want 0.30", rev.String())
	}
}

func mustDec(t *testing.T, s string) runtime.Decimal {
	t.Helper()
	d, err := runtime.ParseDecimal(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var _ = errors.Is

// ---- the rest of docs/COMPLEX-QUERIES.md, against a real server -------------

// date_trunc grouping: the case that used to force raw SQL.
func TestGroupByExpression(t *testing.T) {
	ctx := context.Background()
	svc := orders.New(pool)
	cust, _ := seed(t, "SKU-DAILY", "4.00", 60)
	for i := 0; i < 4; i++ {
		if _, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-DAILY", Quantity: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := order.New().AllDaily(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no day buckets")
	}
	// A day bucket must be midnight UTC — that IS date_trunc.
	for _, d := range got {
		h, m, s := d.Day.UTC().Clock()
		if h != 0 || m != 0 || s != 0 {
			t.Errorf("bucket %s is not truncated to a day", d.Day)
		}
	}
	// The window function ranked them, 1-based and dense from the top.
	if got[0].Rank == 0 {
		t.Error("row_number() produced 0; window functions are 1-based")
	}
}

// FILTER is not decoration: it must count fewer rows than the unfiltered count
// when the predicate excludes some.
func TestAggregateFilter(t *testing.T) {
	ctx := context.Background()
	svc := orders.New(pool)
	cust, _ := seed(t, "SKU-FILTER", "80.00", 40)
	// One big order (80.00 >= 50) and one small (8.00 < 50).
	if _, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-FILTER", Quantity: 1}}); err != nil {
		t.Fatal(err)
	}
	cust2, _ := seed(t, "SKU-SMALL", "8.00", 40)
	if _, err := svc.PlaceOrder(ctx, cust2, []orders.LineRequest{{SKU: "SKU-SMALL", Quantity: 1}}); err != nil {
		t.Fatal(err)
	}

	got, err := order.New().AllByStatus(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	var total, big int64
	for _, g := range got {
		total += g.Orders
		big += g.BigOrders
	}
	if big >= total {
		t.Errorf("FILTER counted %d of %d — it filtered nothing", big, total)
	}
	if big == 0 {
		t.Error("FILTER counted nothing; the test proves nothing")
	}

	// And it must agree with the same FILTER written by hand.
	var want int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE total >= 50.00) FROM orders`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if big != want {
		t.Errorf("storm's FILTER counted %d, hand-written SQL counted %d", big, want)
	}
}

// HAVING filters GROUPS, after aggregation — a different question from a
// call-site Where, which filters the rows going in.
func TestAggregateHaving(t *testing.T) {
	ctx := context.Background()
	got, err := order.New().AllByStatus(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if g.Orders <= 0 {
			t.Errorf("HAVING count(*) > 0 let through a group with %d rows", g.Orders)
		}
	}
}

// GROUPING SETS: per-status, per-day and the grand total in ONE pass. The
// grouping columns must be NULLABLE, and GROUPING() must say which NULL is a
// subtotal rather than data.
func TestGroupingSets(t *testing.T) {
	ctx := context.Background()
	svc := orders.New(pool)
	cust, _ := seed(t, "SKU-FACET", "6.00", 60)
	if _, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-FACET", Quantity: 2}}); err != nil {
		t.Fatal(err)
	}

	got, err := order.New().AllFacets(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 3 {
		t.Fatalf("got %d facet rows; expected per-status + per-day + a grand total", len(got))
	}

	var grand, byStatus, byDay int
	for _, r := range got {
		_, hasStatus := r.Status.Get()
		_, hasDay := r.Day.Get()
		switch {
		case !hasStatus && !hasDay:
			grand++
			if r.StatusIsSubtotal != 1 {
				t.Error("the grand-total row does not report itself as a subtotal")
			}
		case hasStatus && !hasDay:
			byStatus++
			if r.StatusIsSubtotal != 0 {
				t.Error("a real status was reported as a subtotal")
			}
		case !hasStatus && hasDay:
			byDay++
		}
	}
	if grand != 1 {
		t.Errorf("got %d grand-total rows, want exactly 1", grand)
	}
	if byStatus == 0 || byDay == 0 {
		t.Errorf("grouping sets produced %d status rows and %d day rows", byStatus, byDay)
	}

	// One pass must equal three separate queries.
	var wantStatus, wantDay int
	pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT status FROM orders GROUP BY status) s`).Scan(&wantStatus)
	pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT date_trunc('day', placed_at) FROM orders
	                     GROUP BY 1) d`).Scan(&wantDay)
	if byStatus != wantStatus {
		t.Errorf("grouping sets produced %d status groups, a plain GROUP BY produces %d", byStatus, wantStatus)
	}
	if byDay != wantDay {
		t.Errorf("grouping sets produced %d day groups, a plain GROUP BY produces %d", byDay, wantDay)
	}
}

// Every aggregation must be stably ordered — a report that shuffles between
// requests is unusable, and PostgreSQL promises nothing without ORDER BY.
func TestEveryAggregationIsStablyOrdered(t *testing.T) {
	ctx := context.Background()
	first, err := order.New().AllFacets(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := order.New().AllFacets(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(first) {
			t.Fatalf("run %d: %d rows, first: %d", i, len(again), len(first))
		}
		for j := range again {
			a, _ := again[j].Status.Get()
			f, _ := first[j].Status.Get()
			if a != f {
				t.Fatalf("run %d row %d differs — the grouping-set result is unordered", i, j)
			}
		}
	}
}

// A duplicate email is a 409 the client can act on, not a 500. Before storm
// classified constraint violations this arrived as an opaque driver error and
// the handler had to decode a SQLSTATE to tell the difference.
func TestDuplicateIsAConflictNotAFailure(t *testing.T) {
	ctx := context.Background()
	seed(t, "SKU-DUP", "1.00", 5)

	// The same email again: customers.email is UNIQUE.
	nc := customer.Create()
	nc.SetEmail("SKU-DUP@example.com")
	nc.SetName("Ada again")
	_, err := nc.Insert(ctx, ex)
	if err == nil {
		t.Fatal("a duplicate email was accepted; the unique constraint did nothing")
	}
	if !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Fatalf("errors.Is(err, ErrUniqueViolation) is false: %v", err)
	}
	var ce *runtime.ConstraintError
	if !errors.As(err, &ce) {
		t.Fatalf("not a ConstraintError: %v", err)
	}
	if ce.Constraint == "" {
		t.Error("the error does not name which constraint was violated")
	}
	// And storm's own text does not carry the address.
	if strings.Contains(ce.Error(), "SKU-DUP@example.com") {
		t.Errorf("storm's error text leaked a bound value: %s", ce.Error())
	}
	// A check violation is a different answer, and must not be confused with it.
	if errors.Is(err, runtime.ErrCheckViolation) {
		t.Error("a unique violation also matched ErrCheckViolation")
	}
}

// The database refuses to oversell even if the application forgets to ask —
// and that refusal arrives as a typed check violation.
func TestCheckConstraintIsTyped(t *testing.T) {
	ctx := context.Background()
	_, prodID := seed(t, "SKU-CHECK", "1.00", 3)
	// reserved <= on_hand is a CHECK on stock_items.
	_, err := pool.Exec(ctx, `UPDATE stock_items SET reserved = on_hand + 1 WHERE product_id = $1`, prodID)
	if err == nil {
		t.Skip("the check did not fire; nothing to classify")
	}
	// Raw pool.Exec is pgx's, not storm's — go through the port.
	if _, err := ex.Exec(ctx,
		`UPDATE stock_items SET reserved = on_hand + 1 WHERE product_id = $1`, []any{prodID}); err != nil {
		if !errors.Is(err, runtime.ErrCheckViolation) {
			t.Errorf("a CHECK violation was not classified: %v", err)
		}
	} else {
		t.Error("the CHECK did not refuse an oversell")
	}
}
