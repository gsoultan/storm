package storm

import (
	"context"

	"github.com/gsoultan/storm/runtime"
)

// The transaction vocabulary, re-exported here for the same reason Decimal and
// Interval are: an adopter declaring a model already imports storm, and making
// them import runtime as well to name the thing their repository method takes
// would put a package documented as "the thin runtime" in the signature of
// every piece of application code that owns a transaction.
//
// Aliases rather than wrappers. storm.Tx and runtime.Tx must be the SAME type
// or an adapter satisfying one would not satisfy the other, and every call
// site would need a conversion that does nothing.
type (
	// Executor is the driver port: what every generated call takes.
	Executor = runtime.Executor

	// Tx is a transaction — an Executor plus Commit and Rollback. Every
	// adapter's transaction satisfies it, so a helper written over this one
	// runs on PostgreSQL, MySQL, SQL Server and anything behind database/sql.
	Tx = runtime.Tx

	// DB is an Executor that can Begin. A pool, in every adapter that has one.
	DB = runtime.DB
)

// ErrTxDone is returned by a transaction that has already committed or rolled
// back, whichever adapter it came from.
var ErrTxDone = runtime.ErrTxDone

// InTx runs fn inside one transaction and commits it, or rolls back and
// returns why — including when fn panics. See runtime.InTx.
//
//	err := storm.InTx(ctx, db, func(ex storm.Executor) error {
//	    if _, err := acct.Mutate(from, acct.BalanceSub(n)).Exec(ctx, ex); err != nil {
//	        return err
//	    }
//	    _, err := acct.Mutate(to, acct.BalanceAdd(n)).Exec(ctx, ex)
//	    return err
//	})
func InTx(ctx context.Context, db DB, fn func(ex Executor) error) error {
	return runtime.InTx(ctx, db, fn)
}

// Retryable reports whether an error means "run the whole transaction again" —
// a serialization failure or a deadlock, and nothing else. A lock that was not
// available is NOT retryable: someone else holds the row, and a retry loop on
// it is a spin.
func Retryable(err error) bool { return runtime.Retryable(err) }
