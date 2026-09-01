package orders

import (
	"context"
	"time"

	"github.com/go-kit/kit/endpoint"
)

// Endpoints is the Go kit middle layer: one endpoint.Endpoint per business
// method, so rate limiting, tracing and circuit breaking wrap a method rather
// than an HTTP route.
type Endpoints struct {
	Catalogue    endpoint.Endpoint
	PlaceOrder   endpoint.Endpoint
	GetOrder     endpoint.Endpoint
	DailyRevenue endpoint.Endpoint
}

func MakeEndpoints(s Service) Endpoints {
	return Endpoints{
		Catalogue:    makeCatalogue(s),
		PlaceOrder:   makePlaceOrder(s),
		GetOrder:     makeGetOrder(s),
		DailyRevenue: makeDailyRevenue(s),
	}
}

type (
	catalogueRequest  struct{}
	catalogueResponse struct {
		Items []CatalogueItem `json:"items"`
	}

	placeOrderRequest struct {
		CustomerID string        `json:"customer_id"`
		Items      []LineRequest `json:"items"`
	}

	getOrderRequest struct {
		OrderID string `json:"order_id"`
	}

	dailyRevenueRequest struct {
		Since time.Time `json:"since"`
	}
	dailyRevenueResponse struct {
		Days []RevenueDay `json:"days"`
	}
)

func makeCatalogue(s Service) endpoint.Endpoint {
	return func(ctx context.Context, _ any) (any, error) {
		items, err := s.Catalogue(ctx)
		if err != nil {
			return nil, err
		}
		return catalogueResponse{Items: items}, nil
	}
}

func makePlaceOrder(s Service) endpoint.Endpoint {
	return func(ctx context.Context, req any) (any, error) {
		r := req.(placeOrderRequest)
		return s.PlaceOrder(ctx, r.CustomerID, r.Items)
	}
}

func makeGetOrder(s Service) endpoint.Endpoint {
	return func(ctx context.Context, req any) (any, error) {
		return s.GetOrder(ctx, req.(getOrderRequest).OrderID)
	}
}

func makeDailyRevenue(s Service) endpoint.Endpoint {
	return func(ctx context.Context, req any) (any, error) {
		days, err := s.DailyRevenue(ctx, req.(dailyRevenueRequest).Since)
		if err != nil {
			return nil, err
		}
		return dailyRevenueResponse{Days: days}, nil
	}
}
