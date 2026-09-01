package orders

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	httptransport "github.com/go-kit/kit/transport/http"
)

// NewHTTPHandler wires the endpoints to routes. Nothing below this line knows
// about storm, and nothing above it knows about HTTP.
func NewHTTPHandler(e Endpoints) http.Handler {
	opts := []httptransport.ServerOption{
		httptransport.ServerErrorEncoder(encodeError),
	}
	mux := http.NewServeMux()
	mux.Handle("/catalogue", httptransport.NewServer(
		e.Catalogue, decodeCatalogue, encodeResponse, opts...))
	mux.Handle("/orders", httptransport.NewServer(
		e.PlaceOrder, decodePlaceOrder, encodeResponse, opts...))
	mux.Handle("/orders/", httptransport.NewServer(
		e.GetOrder, decodeGetOrder, encodeResponse, opts...))
	mux.Handle("/reports/revenue", httptransport.NewServer(
		e.DailyRevenue, decodeDailyRevenue, encodeResponse, opts...))
	return mux
}

func decodeCatalogue(_ context.Context, _ *http.Request) (any, error) {
	return catalogueRequest{}, nil
}

func decodePlaceOrder(_ context.Context, r *http.Request) (any, error) {
	var req placeOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	return req, nil
}

func decodeGetOrder(_ context.Context, r *http.Request) (any, error) {
	id := strings.TrimPrefix(r.URL.Path, "/orders/")
	if id == "" {
		return nil, errors.New("missing order id")
	}
	return getOrderRequest{OrderID: id}, nil
}

func decodeDailyRevenue(_ context.Context, r *http.Request) (any, error) {
	since := time.Now().UTC().AddDate(0, 0, -30)
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			return nil, err
		}
		since = t
	}
	return dailyRevenueRequest{Since: since}, nil
}

func encodeResponse(ctx context.Context, w http.ResponseWriter, resp any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	return json.NewEncoder(w).Encode(resp)
}

// encodeError maps business failures to status codes. A stale write is 409 and
// explicitly retryable — the client is being told it lost a race, not that it
// sent something wrong.
func encodeError(_ context.Context, err error, w http.ResponseWriter) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNoSuchProduct), errors.Is(err, ErrNoSuchOrder):
		code = http.StatusNotFound
	case errors.Is(err, ErrOutOfStock):
		code = http.StatusConflict
	case errors.Is(err, ErrConcurrentHold), errors.Is(err, ErrDuplicate), errors.Is(err, ErrRetry):
		code = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error":     err.Error(),
		"retryable": errors.Is(err, ErrConcurrentHold) || errors.Is(err, ErrRetry),
	})
}
