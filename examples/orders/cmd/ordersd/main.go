// Command ordersd is the service.
//
//	STORM_DSN=postgres://... go run ./cmd/ordersd
//
// Note what is NOT here: no cmd/storm, no model.All(). `storm generate` found
// the models by parsing and wrote its own bootstrap.
package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-kit/log"
	"github.com/gsoultan/storm/runtime/pgxdrv"

	"example.com/orders/orders"
)

func main() {
	logger := log.NewLogfmtLogger(log.NewSyncWriter(os.Stderr))
	logger = log.With(logger, "ts", log.DefaultTimestampUTC, "svc", "orders")

	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		logger.Log("fatal", "STORM_DSN is unset")
		os.Exit(1)
	}
	ctx := context.Background()

	// storm's constructor, so the fast parameter encoders are installed.
	// Everything else about the pool is ordinary pgx.
	pool, err := pgxdrv.NewPool(ctx, dsn)
	if err != nil {
		logger.Log("fatal", err)
		os.Exit(1)
	}
	defer pool.Close()

	var svc orders.Service = orders.New(pool)
	svc = orders.LoggingMiddleware{Logger: logger, Next: svc}

	srv := &http.Server{
		Addr:              envOr("ADDR", ":8080"),
		Handler:           orders.NewHTTPHandler(orders.MakeEndpoints(svc)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Log("listening", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Log("fatal", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	logger.Log("stopped", true)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
