// Package model is the whole schema: five structs, no tags, no DSL.
//
// storm finds these by parsing — there is no All() list to maintain and no
// bootstrap main. Run `storm models` to see what it concluded.
package model

import (
	"time"

	"github.com/gsoultan/storm"
)

// Status is a Postgres enum. Constants are not discoverable through
// reflection, so the type lists them.
type Status string

const (
	StatusPending   Status = "pending"
	StatusPaid      Status = "paid"
	StatusShipped   Status = "shipped"
	StatusCancelled Status = "cancelled"
)

func (Status) EnumValues() []string {
	return []string{"pending", "paid", "shipped", "cancelled"}
}

// Audited is a MIXIN — shared columns, embedded into the models that want
// them. It is exported and has a Schema method, exactly like a model; storm
// classifies it as a mixin because something embeds it.
type Audited struct {
	UpdatedBy string
}

func (a *Audited) Schema(t *storm.Table) { t.Col(&a.UpdatedBy).Size(64).Default("'system'") }

// Product is the catalogue.
type Product struct {
	storm.Model

	SKU    string
	Name   string
	Price  storm.Decimal
	Active bool

	// Full-text search. A GENERATED column, so PostgreSQL keeps it in step
	// with name and sku and nothing in Go can write it — and storm keeps it
	// out of Row, because a tsvector is index support, not data.
	Search storm.TSVector
}

func (p *Product) Schema(t *storm.Table) {
	t.Col(&p.SKU).Size(32).Unique()
	t.Col(&p.Name).Size(200)
	// numeric, not float: a float64 cannot represent 0.10, and an accounting
	// system that rounds is a defect rather than a tolerance.
	t.Col(&p.Price).Numeric(12, 2)
	t.Col(&p.Active).Default("true")
	t.Check(storm.RawSQL(`price >= 0`))
	t.Col(&p.Search).
		Generated(storm.RawSQL(`to_tsvector('english', coalesce(name,'') || ' ' || coalesce(sku,''))`)).
		Index()
}

// Card is the list-endpoint read: three columns instead of the row, which is a
// covering index away from an index-only scan.
func (p *Product) Projections(pr *storm.Projections) {
	pr.Named("Card", &p.SKU, &p.Name, &p.Price)
}

// StockItem is the reservable inventory.
//
// Version is what makes concurrent checkout correct: two requests that read
// the same row and both try to reserve it cannot both win. The loser is told
// so, rather than silently overselling.
type StockItem struct {
	storm.Model
	Audited

	Product  Product
	OnHand   int32
	Reserved int32
	Version  int32
}

func (s *StockItem) Schema(t *storm.Table) {
	t.Col(&s.Product).OnDelete(storm.Cascade).Unique()
	// Reserved is NOT NULL with no natural starting value from the caller, so
	// it needs a DATABASE default: a masked insert omits what was never
	// assigned, and "omitted" must mean 0 here rather than NULL.
	t.Col(&s.Reserved).Default("0")
	t.Col(&s.Version).Default("0").Version()
	// The database refuses to oversell even if the application forgets to ask.
	t.Check(storm.RawSQL(`on_hand >= 0`))
	t.Check(storm.RawSQL(`reserved >= 0`))
	t.Check(storm.RawSQL(`reserved <= on_hand`))
}

// Booking is the scheduling case: a room reserved for a period. The overlap
// question is answered by the DATABASE, with a GiST index, and two concurrent
// bookings for the same room cannot both commit.
type Booking struct {
	storm.Model

	Room   int32
	Guest  string
	During storm.TstzRange
}

func (b *Booking) Schema(t *storm.Table) {
	t.Col(&b.Guest).Size(200)
	// EXCLUDE USING gist (room WITH =, during WITH &&): no two rows may share
	// a room AND overlap in time. Storing two timestamps and checking in Go
	// loses the boundary cases and races anyway.
	t.Exclude(
		storm.With(&b.Room, storm.OpEq),
		storm.With(&b.During, storm.OpOverlaps),
	)
}

// Customer places orders.
type Customer struct {
	storm.Model

	Email string
	Name  string

	Orders []Order
}

func (c *Customer) Schema(t *storm.Table) { t.Col(&c.Email).Size(320).Unique() }

// Order is the aggregate root.
type Order struct {
	storm.Model
	Audited

	Customer Customer
	Status   Status
	Total    storm.Decimal
	PlacedAt time.Time

	Lines []OrderLine
}

func (o *Order) Schema(t *storm.Table) {
	t.Col(&o.Total).Numeric(12, 2)
	t.Col(&o.Status).Default("'pending'")
	t.Index(&o.Customer, storm.Desc(&o.PlacedAt))
	t.Check(storm.RawSQL(`total >= 0`))
}

// Aggregates: the reporting reads, declared. Predicates still compose at the
// call site — what is fixed here is the GROUP BY and the SELECT list, which is
// the part that cannot be enumerated if it is built at run time.
func (o *Order) Aggregates(a *storm.Aggregates) {
	// Plain grouping, with a FILTER and a HAVING. The handle each declaration
	// returns is how a later clause refers to it — checked by the compiler
	// rather than by a string lookup.
	byStatus := a.Named("ByStatus")
	byStatus.By(&o.Status)
	orders := byStatus.Count("Orders")
	byStatus.Count("BigOrders").Filter(a.Gte(&o.Total, a.Lit(mustDec("50.00"))))
	byStatus.Sum(&o.Total, "Revenue")
	byStatus.Avg(&o.Total, "AvgOrder")
	byStatus.Min(&o.PlacedAt, "FirstOrderAt")
	byStatus.Max(&o.PlacedAt, "LastOrderAt")
	byStatus.Having(a.Gt(orders, 0))

	// Grouping by an EXPRESSION: the date bucketing that used to need raw SQL.
	// A window over the grouped rows ranks each day by revenue, and Lag gives
	// the previous day's — both without a self-join.
	daily := a.Named("Daily")
	day := daily.ByExpr("Day", a.DateTrunc("day", &o.PlacedAt))
	daily.Count("Orders")
	revenue := daily.Sum(&o.Total, "Revenue")
	// Ranked by the day's REVENUE, not by placed_at: a window over grouped
	// rows sees one row per group, so it may only name a grouping expression
	// or an aggregate. storm refuses the other form by name.
	daily.RowNumber("Rank", a.Over().OrderByDesc(revenue))
	daily.Lag(revenue, "PrevRevenue", a.Over().OrderByAsc(day))

	// GROUPING SETS: per-status, per-day, and the grand total in ONE pass
	// instead of three queries. Every grouping column is nullable here — a
	// subtotal row carries NULL for what it aggregated over — and GroupingOf
	// says which NULL is which.
	facets := a.Named("Facets")
	facets.By(&o.Status)
	facets.ByExpr("Day", a.DateTrunc("day", &o.PlacedAt))
	facets.Sets([]string{"Status"}, []string{"Day"}, nil)
	facets.Count("Orders")
	facets.Sum(&o.Total, "Revenue")
	facets.GroupingOf("StatusIsSubtotal", &o.Status)

	// Grouped by customer — the CTE the VsLifetime join materialises.
	byCustomer := a.Named("ByCustomer")
	byCustomer.By(&o.Customer)
	byCustomer.Count("Orders")
	byCustomer.Sum(&o.Total, "Lifetime")

	// No grouping: the whole table as one row.
	totals := a.Named("Totals")
	totals.Count("Orders")
	totals.Sum(&o.Total, "Revenue")
}

func mustDec(s string) storm.Decimal {
	d, err := storm.ParseDecimal(s)
	if err != nil {
		panic(err)
	}
	return d
}

// Joins: reads that project ACROSS tables. A join answers a question and
// returns a flat row; when you want the entities, that is a plan.
func (o *Order) Joins(j *storm.Joins) {
	var c Customer

	// The everyday one: orders with the customer's details. The FK relation
	// says how to join, so there is nothing to spell.
	j.Named("WithCustomer").
		Inner(&c, &o.Customer).
		Take(&o.ID, "OrderID").
		Take(&o.Status, "Status").
		Take(&o.Total, "Total").
		Take(&o.PlacedAt, "PlacedAt").
		Take(&c.Email, "CustomerEmail").
		Take(&c.Name, "CustomerName").
		// A declared filter the caller cannot widen: cancelled orders are
		// never part of this read, whatever a call site asks for.
		Where(j.Ne(&o.Status, string(StatusCancelled))).
		OrderDesc(&o.PlacedAt)

	// A CTE: each customer's lifetime spend, computed ONCE, joined against.
	// The alternative is a correlated subquery per row.
	j.Named("VsLifetime").
		With("spend", &Order{}, "ByCustomer").
		Inner(&c, &o.Customer).
		LeftWith("spend", j.OnCols("spend", "customer_id", &c.ID)).
		Take(&o.ID, "OrderID").
		Take(&o.Total, "Total").
		Take(&c.Email, "CustomerEmail").
		TakeFrom("spend", "Lifetime", "Lifetime").
		TakeFrom("spend", "Orders", "LifetimeOrders").
		OrderDesc(&o.PlacedAt)
}

// No Plans method here on purpose. Every relation already gets a plan of its
// own — store.OrderWithLines() loads an order and its lines in exactly two
// round trips, whatever the line count, and reading o.Lines off a plain
// order.Row does not compile. Declaring p.Named("WithLines") would collide
// with that free one; declaring plans is for COMBINATIONS the automatic tier
// does not cover, and a single relation is not a combination.

// OrderLine is a priced line. UnitPrice is copied, never joined: an order is a
// record of what was charged, and a later price change must not rewrite history.
type OrderLine struct {
	storm.Model

	Order     Order
	Product   Product
	Quantity  int32
	UnitPrice storm.Decimal
}

func (l *OrderLine) Schema(t *storm.Table) {
	t.Col(&l.Order).OnDelete(storm.Cascade)
	t.Col(&l.UnitPrice).Numeric(12, 2)
	t.Check(storm.RawSQL(`quantity > 0`))
}
