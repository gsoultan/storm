package runtime

import "errors"

// ValueRows is Rows from a driver that decodes before storm can see the wire.
//
// database/sql is that shape: it hands back driver.Value, and re-encoding what
// it just decoded so RawValues could return something would be slower than
// reading the values. runtime/sqldrv's rows are this, and a package generated
// for a target whose driver is one of those — Oracle's — reads Values where
// every other generated package reads RawValues. Which one is decided at
// GENERATE time, like every other dialect decision.
//
// A SEPARATE interface rather than a method on Rows, because of the promise in
// docs/STABILITY.md. Rows is part of the port, and a method added to it is a
// method every adopter-written Rows — a test fake, a decorator — no longer
// compiles without. It briefly was one, unreleased, and apidiff called it what
// it was: an incompatible change to v1. This way the port is what v1.1.0 said
// it was, and only the adapters that have values say so.
//
// What it costs: one interface assertion per QUERY, never per row, made by the
// generated package through AsValueRows. And a decorator that WRAPS Rows,
// rather than passing them through as CountingExecutor does, has to forward
// Values to serve a value-shaped package — which it learns from ErrByteRows,
// not from a scan of nil.
type ValueRows interface {
	Rows

	// Values is the decoded row, valid until the next Next. The elements are
	// driver.Value: int64, float64, bool, []byte, string, time.Time, or nil. A
	// NULL is nil, which is what keeps "absent" and "zero" distinct — the same
	// distinction Oracle's empty string destroys one layer up.
	Values() []any
}

// ErrByteRows is what a package generated for a value-shaped target returns
// when its Executor hands back byte-shaped rows: the generated code and the
// adapter were built for different drivers.
//
// It names no query, unlike a statement's own errors, because it is not about
// one. Every read in the package fails the same way at the first, and the fix
// is where the Executor is constructed.
var ErrByteRows = errors.New("storm: this package was generated for a driver that " +
	"decodes values — database/sql, as Oracle's is — and the Executor returned " +
	"wire-byte rows; construct the Executor with runtime/sqldrv")

// AsValueRows is how a package generated for the value shape takes the result
// of a Query. It accepts the call's two results whole, so the generated read
// is the Query call wrapped rather than a second statement:
//
//	rows, err := runtime.AsValueRows(ex.Query(ctx, st.SQL, args))
//
// Rows of the wrong shape are closed here, because the caller never sees them
// to close.
func AsValueRows(r Rows, err error) (ValueRows, error) {
	if err != nil {
		return nil, err
	}
	if v, ok := r.(ValueRows); ok {
		return v, nil
	}
	if r != nil {
		r.Close()
	}
	return nil, ErrByteRows
}
