// Package hooks is storm's two kinds of callback: statement hooks, which see
// every statement an Executor sends, and lifecycle hooks, which a generated
// table package runs around its own writes and reads.
//
// # Statement hooks
//
// Wrap decorates an Executor, the way runtime.CountingExecutor and qcache do,
// and calls Before and After around every statement it sends. This is the
// seam for logging, tracing and metrics: one place sees every query, insert,
// bulk load and batch, whatever generated package issued it.
//
//	db := hooks.WrapDB(pool, hooks.Hooks{
//	    Before: func(ctx context.Context, s *hooks.Statement) (context.Context, error) {
//	        ctx, _ = tracer.Start(ctx, s.Kind.String())
//	        return ctx, nil
//	    },
//	    After: func(ctx context.Context, s *hooks.Statement) {
//	        trace.SpanFromContext(ctx).End()
//	        log.Debug("sql", "kind", s.Kind, "sql", s.SQL, "took", s.Duration, "err", s.Err)
//	    },
//	})
//
// An Executor that is not wrapped pays nothing: no branch, no allocation.
// A wrapped one allocates one Statement per statement it sends.
//
// # Lifecycle hooks
//
// Every generated table package with a primary key declares
//
//	var Hooks hooks.Lifecycle[Ins, Mut, Row]
//
// and its Insert, Update, Delete and full-row reads call the fields that are
// set. See Lifecycle for exactly which calls run which hook.
package hooks

import (
	"context"

	"github.com/gsoultan/storm/runtime"
)

// Hooks are the statement callbacks Wrap installs. Either may be nil.
//
// Before runs before the statement is sent. The context it returns is the one
// the statement runs with and the one After receives, so a span started in
// Before is the span the driver's work happens under. An error from Before
// stops the statement: nothing is sent, After is not called, and the error is
// returned to the caller as it is — which makes Before a guard as well as an
// observer (refusing writes on a read replica, say).
//
// After runs once the statement has finished, with Duration, Rows and Err
// filled in. For a Query that is when the caller closes the result, not when
// Query returns, so Duration covers reading the rows and Rows counts them.
//
// Statement.Args are the bound values. Logging them logs whatever the caller
// bound, which can be a password or a token: keep them at debug level or
// leave them out.
type Hooks struct {
	Before func(ctx context.Context, s *Statement) (context.Context, error)
	After  func(ctx context.Context, s *Statement)
}

// Wrap returns ex with h installed. The result keeps ex's shape: a runtime.Tx
// comes back as a Tx and a runtime.DB as a DB (whose StartTx hands back a
// wrapped Tx), so code that asks for either still gets it.
//
// With neither hook set Wrap returns ex itself, so hooks configured off cost
// nothing.
func Wrap(ex runtime.Executor, h Hooks) runtime.Executor {
	if h.Before == nil && h.After == nil {
		return ex
	}
	switch v := ex.(type) {
	case runtime.Tx:
		return &tx{executor{inner: v, h: h}, v}
	case runtime.DB:
		return &db{executor{inner: v, h: h}, v}
	}
	return &executor{inner: ex, h: h}
}

// WrapDB is Wrap for a pool, typed so the result can be handed to
// runtime.InTx. Transactions it starts are wrapped too.
func WrapDB(d runtime.DB, h Hooks) runtime.DB {
	if h.Before == nil && h.After == nil {
		return d
	}
	return &db{executor{inner: d, h: h}, d}
}

// WrapTx is Wrap for a transaction the caller already holds.
func WrapTx(t runtime.Tx, h Hooks) runtime.Tx {
	if h.Before == nil && h.After == nil {
		return t
	}
	return &tx{executor{inner: t, h: h}, t}
}
