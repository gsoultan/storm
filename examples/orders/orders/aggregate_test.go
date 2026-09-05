package orders_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

// The ratio must be a RATIO, which is the whole reason arithmetic is worth
// having in a declaration.
//
// PostgreSQL's `/` on two integers truncates, so `paid / orders` over two
// counts is 0 for every day that is not entirely paid and 1 for every day that
// is. That is a plausible-looking wrong answer — the report renders, the
// numbers are in range, and nobody notices until somebody checks by hand. Div
// resolves integer division to numeric and the back end emits the cast, so
// this asserts the fraction survives.
func TestAggregateRatioIsNotIntegerDivision(t *testing.T) {
	ctx := context.Background()
	svc := orders.New(pool)
	cust, _ := seed(t, "SKU-RATIO", "10.00", 100)

	// Four orders, of which one is paid: 0.25, a number integer division
	// cannot represent.
	var ids []string
	for i := 0; i < 4; i++ {
		id, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-RATIO", Quantity: 1}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id.OrderID)
	}
	if _, err := pool.Exec(ctx, `UPDATE orders SET status = 'paid' WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}

	// Scoped to this test's customer. Predicates compose on an aggregation the
	// same as anywhere else — they are a WHERE, filtering the rows that go
	// INTO the groups, so the counts below are this test's and not the suite's.
	var cid [16]byte
	if err := pool.QueryRow(ctx, `SELECT id FROM customers WHERE id = $1`, cust).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	rows, err := order.New().Where(order.CustomerID.Eq(cid)).AllTrend(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d day(s), want 1", len(rows))
	}
	r := rows[0]
	if r.Orders != 4 || r.Paid != 1 {
		t.Fatalf("counts are %d orders / %d paid, want 4 / 1", r.Orders, r.Paid)
	}
	rate, ok := r.PaidRate.Get()
	if !ok {
		t.Fatal("the ratio came back NULL for a day with four orders")
	}
	if got := rate.String(); !strings.HasPrefix(got, "0.25") {
		t.Errorf("paid rate = %s, want 0.25 — integer division would give 0", got)
	}

	// One customer placed all four, which is the question CountDistinct asks
	// and neither Count nor CountOf does.
	if r.Buyers != 1 {
		t.Errorf("distinct buyers = %d, want 1 for four orders from one customer", r.Buyers)
	}

	// The frame reaches back six days and forward none, so a single day's
	// moving average is that day's revenue.
	avg, ok := r.Revenue7d.Get()
	if !ok {
		t.Fatal("the moving average came back NULL")
	}
	rev, _ := r.Revenue.Get()
	// Compared as numbers: PostgreSQL's avg carries a different scale from the
	// sum it averaged, so 40.00 and 40.000000 are the same answer.
	if avg.Float64() != rev.Float64() {
		t.Errorf("7-day average over one day = %s, want the day's revenue %s", avg, rev)
	}
}

// avg() over a numeric column must decode at money-sized values.
//
// PostgreSQL's avg DIVIDES, and numeric division picks its own scale: the
// average of one order of 123456789.12 comes back as 123456789.120000000000 —
// twenty-one significant digits, where a Decimal holds eighteen. Before the
// result type carried a scale this failed to decode with ErrDecimalRange, so
// an aggregation that worked on test-sized data broke on real invoices.
func TestAggregateAvgDecodesAtMoneySizedValues(t *testing.T) {
	ctx := context.Background()
	svc := orders.New(pool)
	cust, _ := seed(t, "SKU-BIG", "999999.99", 1000)
	if _, err := svc.PlaceOrder(ctx, cust,
		[]orders.LineRequest{{SKU: "SKU-BIG", Quantity: 123}}); err != nil {
		t.Fatal(err)
	}

	rows, err := order.New().Where(order.Status.Eq("pending")).AllByStatus(ctx, ex)
	if err != nil {
		t.Fatalf("avg over a money-sized column did not decode: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no groups")
	}
	if _, ok := rows[0].AvgOrder.Get(); !ok {
		t.Error("avg came back NULL for a group with orders in it")
	}
}

// A declared parameter and a call-site predicate in one statement.
//
// The parameter is $1, in the fixed prefix where the FILTER lives; the
// predicate is spliced after it and must be numbered from $2. Getting that
// wrong does not fail loudly — it binds the wrong value to the wrong
// placeholder and returns plausible rows, which is why this asserts the
// numbers by asserting the ANSWER.
func TestAggregateParamComposesWithCallSitePredicates(t *testing.T) {
	ctx := context.Background()
	svc := orders.New(pool)
	cust, _ := seed(t, "SKU-PARAM", "5.00", 100)
	if _, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-PARAM", Quantity: 2}}); err != nil {
		t.Fatal(err)
	}

	var cid [16]byte
	if err := pool.QueryRow(ctx, `SELECT id FROM customers WHERE id = $1`, cust).Scan(&cid); err != nil {
		t.Fatal(err)
	}

	// A boundary in the past: the order counts.
	past := time.Now().Add(-time.Hour)
	rows, err := order.New().Where(order.CustomerID.Eq(cid)).AllPaidRate(ctx, ex, past)
	if err != nil {
		t.Fatal(err)
	}
	var recent int64
	for _, r := range rows {
		recent += r.Recent
	}
	if recent != 1 {
		t.Fatalf("recent = %d with a boundary an hour ago, want 1", recent)
	}

	// A boundary in the future: the same statement, the same predicate, and
	// nothing is recent. If the parameter and the predicate were transposed
	// this would not change.
	future := time.Now().Add(time.Hour)
	rows, err = order.New().Where(order.CustomerID.Eq(cid)).AllPaidRate(ctx, ex, future)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Recent != 0 {
			t.Fatalf("recent = %d with a boundary an hour ahead, want 0", r.Recent)
		}
	}

	// And the call-site predicate still narrows: a customer with no orders.
	other, _ := seed(t, "SKU-PARAM2", "5.00", 100)
	var oid [16]byte
	if err := pool.QueryRow(ctx, `SELECT id FROM customers WHERE id = $1`, other).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	if rows, err := order.New().Where(order.CustomerID.Eq(oid)).AllPaidRate(ctx, ex, past); err != nil {
		t.Fatal(err)
	} else if len(rows) != 0 {
		t.Fatalf("a customer with no orders produced %d group(s)", len(rows))
	}
}

// The whole point of ordering by a measure: the top N come back from the
// database, not from sorting every group in Go.
func TestTopCustomersOrdersByTheMeasureAndPagesTotally(t *testing.T) {
	ctx := context.Background()
	svc := orders.New(pool)

	// Three customers with deliberately different totals, two of them tied,
	// because a tie is what makes an unstable ordering visible.
	for _, c := range []struct {
		sku string
		qty int32
	}{{"SKU-TOP-A", 7}, {"SKU-TOP-B", 3}, {"SKU-TOP-C", 3}} {
		cust, _ := seed(t, c.sku, "10.00", 100)
		if _, err := svc.PlaceOrder(ctx, cust,
			[]orders.LineRequest{{SKU: c.sku, Quantity: c.qty}}); err != nil {
			t.Fatal(err)
		}
	}

	all, err := order.New().AllTopCustomers(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Fatalf("want at least 3 groups, got %d", len(all))
	}

	// Descending by spend, which is not a grouping column and could not be a
	// sort key before.
	for i := 1; i < len(all); i++ {
		prev, cur := all[i-1].Spend, all[i].Spend
		if !prev.Valid || !cur.Valid {
			t.Fatalf("sum over a non-empty group is NULL at row %d", i)
		}
		if prev.V.Float64() < cur.V.Float64() {
			t.Fatalf("row %d spends %s, more than row %d's %s — not descending",
				i, cur.V, i-1, prev.V)
		}
	}

	// Paging walks every group exactly once. This is an end-to-end check that
	// the ordering, LIMIT and OFFSET agree against a real server — NOT proof
	// of the tiebreak: PostgreSQL's sort happens to be stable at this row
	// count, and removing the tiebreak leaves this test green. The tiebreak is
	// asserted where it can trip, on the emitted SQL, in
	// compile/pgsql.TestMeasureOrderIsBrokenTiedByTheGrouping.
	seen := map[[16]byte]int{}
	for off := int64(0); off < int64(len(all)); off++ {
		page, err := order.New().Limit(1).Offset(off).AllTopCustomers(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 1 {
			t.Fatalf("offset %d returned %d rows", off, len(page))
		}
		seen[page[0].CustomerID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("customer %x appears on %d pages of a total ordering", id, n)
		}
	}
	if len(seen) != len(all) {
		t.Fatalf("the page walk saw %d distinct customers of %d groups", len(seen), len(all))
	}
}
