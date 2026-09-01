package orders_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"

	"example.com/orders/model"
	"example.com/orders/orders"
	"example.com/orders/store"
	"example.com/orders/store/customer"
	"example.com/orders/store/order"
	"example.com/orders/store/product"
	"example.com/orders/store/stockitem"
)

var (
	pool *pgxpool.Pool
	ex   runtime.Executor
)

const ns = "orders_example"

func TestMain(m *testing.M) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		fmt.Println("STORM_DSN unset; skipping the orders example")
		os.Exit(0)
	}
	ctx := context.Background()

	p, err := pgxdrv.NewPool(ctx, dsn)
	must(err)

	// model → DDL, applied to this example's own namespace. A real project
	// runs `storm diff` and applies the reviewed migration with its own
	// runner; storm itself never applies DDL.
	s, err := storm.Build(&model.Product{}, &model.StockItem{}, &model.Customer{},
		&model.Order{}, &model.OrderLine{}, &model.Booking{})
	must(err)
	_, err = p.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE; CREATE SCHEMA "+ns)
	must(err)
	_, err = p.Exec(ctx, "SET search_path TO "+ns+"; "+pgddl.Create(s))
	must(err)

	cfg := p.Config()
	cfg.ConnConfig.RuntimeParams["search_path"] = ns
	p.Close()
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	must(err)
	defer pool.Close()
	ex = pgxdrv.Pool{P: pool}

	code := m.Run()
	_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE")
	os.Exit(code)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// seed writes a customer and one product with `onHand` units in stock.
func seed(t *testing.T, sku string, price string, onHand int32) (custID string, prodID [16]byte) {
	t.Helper()
	ctx := context.Background()

	nc := customer.Create()
	nc.SetEmail(sku + "@example.com")
	nc.SetName("Ada")
	c, err := nc.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}

	dec, err := storm.ParseDecimal(price)
	if err != nil {
		t.Fatal(err)
	}
	np := product.Create()
	np.SetSku(sku)
	np.SetName("Widget " + sku)
	np.SetPrice(dec)
	p, err := np.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}

	ns := stockitem.Create()
	ns.SetProductID(p.ID)
	ns.SetOnHand(onHand)
	if _, err := ns.Insert(ctx, ex); err != nil {
		t.Fatal(err)
	}
	return formatUUID(c.ID), p.ID
}

func formatUUID(b [16]byte) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out = append(out, '-')
		}
		out = append(out, hexd[v>>4], hexd[v&0x0f])
	}
	return string(out)
}

// The happy path, end to end: reserve stock, write the order graph, read it
// back through the plan.
func TestPlaceAndReadOrder(t *testing.T) {
	svc := orders.New(pool)
	ctx := context.Background()
	cust, _ := seed(t, "SKU-HAPPY", "19.99", 10)

	placed, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-HAPPY", Quantity: 3}})
	if err != nil {
		t.Fatal(err)
	}
	// 3 × 19.99 = 59.97, exactly. Not 59.969999999999999.
	if placed.Total != "59.97" {
		t.Errorf("total = %q, want %q", placed.Total, "59.97")
	}

	got, err := svc.GetOrder(ctx, placed.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 1 || got.Lines[0].SKU != "SKU-HAPPY" || got.Lines[0].Quantity != 3 {
		t.Fatalf("lines = %+v", got.Lines)
	}
	if got.Lines[0].UnitPrice != "19.99" {
		t.Errorf("unit price = %q", got.Lines[0].UnitPrice)
	}
	if got.Status != string(model.StatusPending) {
		t.Errorf("status = %q", got.Status)
	}
}

// The plan's whole claim: two round trips for an order and its lines, however
// many lines there are.
func TestOrderDetailCostsTwoRoundTrips(t *testing.T) {
	svc := orders.New(pool)
	ctx := context.Background()
	cust, _ := seed(t, "SKU-TRIPS", "5.00", 100)

	placed, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-TRIPS", Quantity: 7}})
	if err != nil {
		t.Fatal(err)
	}
	// Counted on the PLAN itself, not on GetOrder, so the number belongs to
	// the plan and not to the handler's extra SKU lookup.
	oid := parseUUID(t, placed.OrderID)
	count := &runtime.CountingExecutor{Inner: ex}
	rows, err := store.OrderWithLines().Where(order.ID.Eq(oid)).All(ctx, count)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Lines) != 1 {
		t.Fatalf("plan returned %d order(s)", len(rows))
	}
	if got := count.RoundTrips(); got != 2 {
		t.Fatalf("the order plan cost %d round trips, want 2", got)
	}
}

func parseUUID(t *testing.T, s string) [16]byte {
	t.Helper()
	var out [16]byte
	clean := strings.ReplaceAll(s, "-", "")
	b, err := hex.DecodeString(clean)
	if err != nil || len(b) != 16 {
		t.Fatalf("%q is not a uuid: %v", s, err)
	}
	copy(out[:], b)
	return out
}

// Overselling must be impossible: ask for more than exists and be refused.
func TestOutOfStockIsRefused(t *testing.T) {
	svc := orders.New(pool)
	ctx := context.Background()
	cust, prodID := seed(t, "SKU-SCARCE", "1.00", 2)

	_, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-SCARCE", Quantity: 5}})
	if !errors.Is(err, orders.ErrOutOfStock) {
		t.Fatalf("err = %v, want ErrOutOfStock", err)
	}
	// And nothing was reserved on the way to failing.
	st, found, err := stockitem.New().Where(stockitem.ProductID.Eq(prodID)).One(ctx, ex)
	if err != nil || !found {
		t.Fatal(err)
	}
	if st.Reserved != 0 {
		t.Errorf("a refused order left %d units reserved", st.Reserved)
	}
}

// A failed line must not leave the earlier lines' reservations behind. This is
// what the transaction is for.
func TestPartialFailureReservesNothing(t *testing.T) {
	svc := orders.New(pool)
	ctx := context.Background()
	cust, goodID := seed(t, "SKU-GOOD", "2.00", 50)
	seed(t, "SKU-THIN", "2.00", 1)

	_, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{
		{SKU: "SKU-GOOD", Quantity: 5},  // succeeds, reserves 5
		{SKU: "SKU-THIN", Quantity: 99}, // fails
	})
	if !errors.Is(err, orders.ErrOutOfStock) {
		t.Fatalf("err = %v, want ErrOutOfStock", err)
	}
	st, found, err := stockitem.New().Where(stockitem.ProductID.Eq(goodID)).One(ctx, ex)
	if err != nil || !found {
		t.Fatal(err)
	}
	if st.Reserved != 0 {
		t.Fatalf("the rolled-back order left %d units reserved on SKU-GOOD", st.Reserved)
	}
}

// The version column, doing its job: N concurrent orders for a stock of N must
// not oversell. Some lose with ErrConcurrentHold; none succeeds beyond stock.
func TestConcurrentReservationsDoNotOversell(t *testing.T) {
	svc := orders.New(pool)
	ctx := context.Background()
	const stock = 20
	const racers = 12
	cust, prodID := seed(t, "SKU-RACE", "1.00", stock)

	var wg sync.WaitGroup
	results := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.PlaceOrder(ctx, cust,
				[]orders.LineRequest{{SKU: "SKU-RACE", Quantity: 2}})
			results[i] = err
		}(i)
	}
	wg.Wait()

	var ok, lost, other int
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, orders.ErrConcurrentHold), errors.Is(err, orders.ErrOutOfStock):
			lost++
		default:
			other++
			t.Errorf("unexpected failure: %v", err)
		}
	}
	if other > 0 {
		t.Fatalf("%d racers failed for the wrong reason", other)
	}
	if ok+lost != racers {
		t.Fatalf("accounted for %d of %d racers", ok+lost, racers)
	}

	st, found, err := stockitem.New().Where(stockitem.ProductID.Eq(prodID)).One(ctx, ex)
	if err != nil || !found {
		t.Fatal(err)
	}
	// The invariant that matters: never more reserved than exists.
	if st.Reserved > st.OnHand {
		t.Fatalf("OVERSOLD: reserved %d of %d on hand", st.Reserved, st.OnHand)
	}
	if int(st.Reserved) != ok*2 {
		t.Fatalf("reserved %d but %d orders succeeded (expected %d)", st.Reserved, ok, ok*2)
	}
	t.Logf("%d/%d orders won, %d lost the race, %d/%d units reserved — nothing oversold",
		ok, racers, lost, st.Reserved, st.OnHand)
}

// The projection: three columns, not the row.
func TestCatalogueUsesTheProjection(t *testing.T) {
	svc := orders.New(pool)
	seed(t, "SKU-CAT", "3.50", 5)
	items, err := svc.Catalogue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range items {
		if it.SKU == "SKU-CAT" {
			found = true
			if it.Price != "3.50" {
				t.Errorf("price = %q, want 3.50", it.Price)
			}
		}
	}
	if !found {
		t.Error("the seeded product is not in the catalogue")
	}
}

// The declared raw query, validated at generate time, running for real.
func TestDailyRevenueReport(t *testing.T) {
	svc := orders.New(pool)
	ctx := context.Background()
	cust, _ := seed(t, "SKU-REV", "10.00", 10)
	if _, err := svc.PlaceOrder(ctx, cust, []orders.LineRequest{{SKU: "SKU-REV", Quantity: 2}}); err != nil {
		t.Fatal(err)
	}
	days, err := svc.DailyRevenue(ctx, time.Now().UTC().AddDate(0, 0, -1))
	if err != nil {
		t.Fatal(err)
	}
	if len(days) == 0 {
		t.Fatal("no revenue rows")
	}
	if !strings.Contains(days[0].Day, "-") || days[0].Orders < 1 {
		t.Fatalf("row = %+v", days[0])
	}
}
