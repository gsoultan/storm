package orders

import (
	"context"
	"time"

	"github.com/go-kit/log"
)

// LoggingMiddleware is the canonical Go kit decorator: same interface in, same
// interface out, so it composes and the service stays unaware.
type LoggingMiddleware struct {
	Logger log.Logger
	Next   Service
}

func (m LoggingMiddleware) Catalogue(ctx context.Context) (items []CatalogueItem, err error) {
	defer func(t time.Time) {
		m.Logger.Log("method", "Catalogue", "n", len(items), "took", time.Since(t), "err", err)
	}(time.Now())
	return m.Next.Catalogue(ctx)
}

func (m LoggingMiddleware) PlaceOrder(ctx context.Context, customerID string, items []LineRequest) (p *Placed, err error) {
	defer func(t time.Time) {
		m.Logger.Log("method", "PlaceOrder", "customer", customerID, "lines", len(items),
			"took", time.Since(t), "err", err)
	}(time.Now())
	return m.Next.PlaceOrder(ctx, customerID, items)
}

func (m LoggingMiddleware) GetOrder(ctx context.Context, orderID string) (d *OrderDetail, err error) {
	defer func(t time.Time) {
		m.Logger.Log("method", "GetOrder", "order", orderID, "took", time.Since(t), "err", err)
	}(time.Now())
	return m.Next.GetOrder(ctx, orderID)
}

func (m LoggingMiddleware) DailyRevenue(ctx context.Context, since time.Time) (d []RevenueDay, err error) {
	defer func(t time.Time) {
		m.Logger.Log("method", "DailyRevenue", "since", since, "took", time.Since(t), "err", err)
	}(time.Now())
	return m.Next.DailyRevenue(ctx, since)
}
