// Package orders is the business layer: the Go kit Service, and the only place
// that knows storm exists.
//
// Endpoints and transports below it deal in plain request/response structs, so
// the persistence choice never leaks into HTTP handlers — the point of Go kit's
// layering, and the reason the storm calls all live here.
package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/runtime"
	"github.com/jackc/pgx/v5/pgxpool"

	"example.com/orders/model"
	"example.com/orders/store"
	"example.com/orders/store/order"
	"example.com/orders/store/orderline"
	"example.com/orders/store/product"
	"example.com/orders/store/stockitem"

	"github.com/gsoultan/storm/runtime/pgxdrv"
)

// Business failures, distinct from infrastructure ones. The transport maps
// these to 4xx; anything else is a 500.
var (
	ErrNoSuchProduct  = errors.New("no such product")
	ErrOutOfStock     = errors.New("insufficient stock")
	ErrConcurrentHold = errors.New("stock changed while reserving; retry")
	ErrNoSuchOrder    = errors.New("no such order")
	// ErrDuplicate is a unique constraint the client can fix by sending
	// something else — a 409, not a 500.
	ErrDuplicate = errors.New("already exists")
	// ErrRetry is a serialization failure or a deadlock: nobody's mistake,
	// and the same request will probably succeed.
	ErrRetry = errors.New("conflicting concurrent transaction; retry")
)

// classify maps storm's typed constraint errors onto this service's own.
//
// Without runtime's classification this would be a type assertion on a pgx
// error and a SQLSTATE comparison, in a business-logic file that has no
// business knowing which driver is underneath.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case runtime.Retryable(err):
		return fmt.Errorf("%w: %v", ErrRetry, err)
	case errors.Is(err, runtime.ErrUniqueViolation):
		var ce *runtime.ConstraintError
		if errors.As(err, &ce) && ce.Constraint != "" {
			return fmt.Errorf("%w (%s)", ErrDuplicate, ce.Constraint)
		}
		return ErrDuplicate
	case errors.Is(err, runtime.ErrForeignKeyViolation):
		return fmt.Errorf("%w: a referenced row is missing", ErrNoSuchProduct)
	}
	return err
}

// Service is the business API.
type Service interface {
	// Catalogue lists sellable products.
	Catalogue(ctx context.Context) ([]CatalogueItem, error)
	// PlaceOrder reserves stock and records an order, atomically.
	PlaceOrder(ctx context.Context, customerID string, items []LineRequest) (*Placed, error)
	// GetOrder returns an order with its lines.
	GetOrder(ctx context.Context, orderID string) (*OrderDetail, error)
	// DailyRevenue is the finance report.
	DailyRevenue(ctx context.Context, since time.Time) ([]RevenueDay, error)
}

type LineRequest struct {
	SKU      string `json:"sku"`
	Quantity int32  `json:"quantity"`
}

type CatalogueItem struct {
	SKU   string `json:"sku"`
	Name  string `json:"name"`
	Price string `json:"price"` // decimal as text: JSON numbers are float64
}

type Placed struct {
	OrderID string `json:"order_id"`
	Total   string `json:"total"`
	Status  string `json:"status"`
}

type OrderDetail struct {
	OrderID  string       `json:"order_id"`
	Status   string       `json:"status"`
	Total    string       `json:"total"`
	PlacedAt time.Time    `json:"placed_at"`
	Lines    []DetailLine `json:"lines"`
}

type DetailLine struct {
	SKU       string `json:"sku"`
	Quantity  int32  `json:"quantity"`
	UnitPrice string `json:"unit_price"`
}

type RevenueDay struct {
	Day     string `json:"day"`
	Orders  int64  `json:"orders"`
	Revenue string `json:"revenue"`
	// Rank is the day's position by revenue, and PrevRevenue the previous
	// day's — both from window functions over the grouped rows, no self-join.
	Rank        int64  `json:"rank"`
	PrevRevenue string `json:"prev_revenue,omitempty"`
}

type service struct {
	pool *pgxpool.Pool
	ex   runtime.Executor
}

// New builds the service over a storm-configured pool.
func New(pool *pgxpool.Pool) Service {
	return &service{pool: pool, ex: pgxdrv.Pool{P: pool}}
}

// Catalogue reads three columns instead of the row. Same builder, same
// predicates, narrower tuple — a covering index away from an index-only scan.
func (s *service) Catalogue(ctx context.Context) ([]CatalogueItem, error) {
	cards, err := product.New().
		Where(product.Active.Eq(true)).
		Order(product.Sku.Asc()).
		AllCard(ctx, s.ex)
	if err != nil {
		return nil, err
	}
	out := make([]CatalogueItem, 0, len(cards))
	for _, c := range cards {
		out = append(out, CatalogueItem{SKU: c.Sku, Name: c.Name, Price: c.Price.String()})
	}
	return out, nil
}

// PlaceOrder is the whole business case.
//
// It reserves stock and records the order in ONE transaction, and it uses the
// version column so two customers racing for the last unit cannot both win.
// The loser is told to retry; nothing is oversold, and no row is silently
// last-write-wins.
func (s *service) PlaceOrder(ctx context.Context, customerID string, items []LineRequest) (*Placed, error) {
	if len(items) == 0 {
		return nil, errors.New("an order needs at least one line")
	}
	cid, err := parseID(customerID)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit succeeds
	// A transaction is just another Executor: the same generated code runs
	// inside one, and a rollback erases everything it did.
	ex := pgxdrv.Tx{T: tx}

	skus := make([]string, 0, len(items))
	for _, it := range items {
		if it.Quantity <= 0 {
			return nil, fmt.Errorf("%s: quantity must be positive", it.SKU)
		}
		skus = append(skus, it.SKU)
	}

	// One query for every product, not one per line.
	prods, err := product.New().Where(product.Sku.In(skus...)).All(ctx, ex, nil)
	if err != nil {
		return nil, err
	}
	bySKU := make(map[string]product.Row, len(prods))
	for _, p := range prods {
		bySKU[p.Sku] = p
	}

	orderID := newID()
	total := storm.Decimal{Scale: 2}
	lines := make([]orderline.Row, 0, len(items))

	for _, it := range items {
		p, ok := bySKU[it.SKU]
		if !ok || !p.Active {
			return nil, fmt.Errorf("%w: %s", ErrNoSuchProduct, it.SKU)
		}

		// Read the stock row, INCLUDING its version.
		st, found, err := stockitem.New().Where(stockitem.ProductID.Eq(p.ID)).One(ctx, ex)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("%w: %s has no stock record", ErrNoSuchProduct, it.SKU)
		}
		if avail := st.OnHand - st.Reserved; avail < it.Quantity {
			return nil, fmt.Errorf("%w: %s has %d available, wanted %d",
				ErrOutOfStock, it.SKU, avail, it.Quantity)
		}

		// Reserve. The UPDATE carries `WHERE version = <what we read>`, so a
		// concurrent reservation that landed between our read and this write
		// matches no row and comes back ErrStaleWrite.
		m := stockitem.Mutate(st)
		m.SetReserved(st.Reserved + it.Quantity)
		m.SetUpdatedBy("orders-service")
		if err := m.Update(ctx, ex); err != nil {
			if errors.Is(err, runtime.ErrStaleWrite) {
				return nil, fmt.Errorf("%w: %s", ErrConcurrentHold, it.SKU)
			}
			return nil, classify(err)
		}

		total = addDecimal(total, mulDecimal(p.Price, it.Quantity))
		lines = append(lines, orderline.Row{
			ID: newID(), OrderID: orderID, ProductID: p.ID,
			Quantity: it.Quantity, UnitPrice: p.Price,
		})
	}

	// The graph write: staged in any order, flushed in foreign-key order, one
	// batch, atomic. The order does not have to exist before its lines are
	// staged — FlushOrder knows orders precede order_lines.
	u := store.NewUnit()
	u.Add(order.Table, order.InsertOp(order.Row{
		ID: orderID, CustomerID: cid, Status: string(model.StatusPending),
		Total: total, PlacedAt: time.Now().UTC(), UpdatedBy: "orders-service",
	}))
	for _, l := range lines {
		u.Add(orderline.Table, orderline.InsertOp(l))
	}
	if _, err := u.Flush(ctx, ex); err != nil {
		return nil, classify(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &Placed{
		OrderID: formatID(orderID),
		Total:   total.String(),
		Status:  string(model.StatusPending),
	}, nil
}

// GetOrder uses the plan the relation tier generated for free: exactly two
// round trips, whatever the line count. Reading detail.Lines off a plain
// order.Row would not compile at all.
func (s *service) GetOrder(ctx context.Context, orderID string) (*OrderDetail, error) {
	oid, err := parseID(orderID)
	if err != nil {
		return nil, err
	}
	rows, err := store.OrderWithLines().Where(order.ID.Eq(oid)).All(ctx, s.ex)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNoSuchOrder
	}
	o := rows[0]

	// Line SKUs: one query for the whole order, never one per line.
	ids := make([][16]byte, 0, len(o.Lines))
	for _, l := range o.Lines {
		ids = append(ids, l.ProductID)
	}
	prods, err := product.New().Where(product.ID.In(ids...)).All(ctx, s.ex, nil)
	if err != nil {
		return nil, err
	}
	sku := make(map[[16]byte]string, len(prods))
	for _, p := range prods {
		sku[p.ID] = p.Sku
	}

	out := &OrderDetail{
		OrderID: formatID(o.ID), Status: o.Status,
		Total: o.Total.String(), PlacedAt: o.PlacedAt,
	}
	for _, l := range o.Lines {
		out.Lines = append(out.Lines, DetailLine{
			SKU: sku[l.ProductID], Quantity: l.Quantity, UnitPrice: l.UnitPrice.String(),
		})
	}
	return out, nil
}

// DailyRevenue is a DECLARED aggregation — no SQL here at all.
//
// It used to be a storm.SQL[T] because the builder could not group by
// date_trunc('day', …). It can now, so the raw query is gone and the report is
// typed, predicate-composable and validated at build time like everything else.
// The window gives each day its rank and the previous day's revenue without a
// self-join.
func (s *service) DailyRevenue(ctx context.Context, since time.Time) ([]RevenueDay, error) {
	rows, err := order.New().
		Where(order.PlacedAt.Gte(since), order.Status.NotEq(string(model.StatusCancelled))).
		AllDaily(ctx, s.ex)
	if err != nil {
		return nil, err
	}
	out := make([]RevenueDay, 0, len(rows))
	for _, r := range rows {
		d := RevenueDay{Day: r.Day.UTC().Format("2006-01-02"), Orders: r.Orders, Rank: r.Rank}
		if rev, ok := r.Revenue.Get(); ok {
			d.Revenue = rev.String()
		}
		if prev, ok := r.PrevRevenue.Get(); ok {
			d.PrevRevenue = prev.String()
		}
		out = append(out, d)
	}
	return out, nil
}
