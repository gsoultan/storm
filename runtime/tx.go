package runtime

import (
	"context"
	"errors"
)

// ErrTxDone is returned by a transaction that has already committed or rolled
// back. Adapters return this exact error so that a caller can test for it
// without naming the adapter it came from — which is the whole point of the
// two interfaces below.
var ErrTxDone = errors.New("storm: the transaction has already finished")

// Tx is a transaction: an Executor, plus the two ways it can end.
//
// ADR-0005 kept Begin, Commit and Rollback OUT of the Executor port and that
// has not changed — the port is still four methods with a budget of five, and
// a decorator still only implements four. What ADR-0005 did not say, and what
// ADR-0011 corrects, is that "out of the port" had been read as "out of the
// library": every adapter grew its own transaction type with its own spelling,
// so a caller who wanted to write one helper over two databases could not name
// the thing they were holding.
//
//	pgx:            tx, _ := pool.Begin(ctx); ex := pgxdrv.Tx{T: tx}
//	mydrv / msdrv:  tx, _ := pool.Begin(ctx)          // *mydrv.Tx, *msdrv.Tx
//	database/sql:   tx, _ := db.BeginTx(ctx, nil); ex := sqldrv.New(tx)
//
// Three spellings, two of which hand back a type the caller has to convert
// before generated code will take it, and one of which cannot be written
// generically at all. Tx is the one name for all of them.
//
// Rollback on a finished transaction returns nil, not ErrTxDone, so that
//
//	defer tx.Rollback(ctx)
//
// beside a commit is the idiom rather than a swallowed error. Commit on a
// finished transaction returns ErrTxDone, because committing twice is a bug
// and there is no shape in which it is not.
type Tx interface {
	Executor

	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// DB is an Executor that can start a transaction: a pool, in every adapter
// that has one.
//
// This is deliberately NOT the Executor port. Generated code takes an
// Executor and must keep taking one, because most things storm is handed are
// not pools — a Tx is not, a pinned Conn is not, a CountingExecutor is not,
// and a test double should not have to pretend. DB is what a CALLER holds and
// names on purpose at the top of a request, which is a different position in
// the program from the one the port occupies.
//
// It is also not a capability to sniff for. Nothing in storm type-asserts an
// Executor to a DB to discover whether it can begin — that is the runtime
// capability sniff ADR-0005 rejected, and it brings the failure mode the rule
// exists to prevent. A caller who needs a transaction declares DB in their own
// signature and the compiler settles it.
type DB interface {
	Executor

	// Begin starts a transaction. The returned Tx is an Executor, so every
	// generated surface takes it unchanged.
	Begin(ctx context.Context) (Tx, error)
}

// InTx runs fn inside one transaction and commits it, or rolls back and
// returns why.
//
// The three ways out are all handled, because the reason to have this function
// at all is that hand-written versions of it usually handle two:
//
//   - fn returns an error   → rollback, return fn's error (not the rollback's)
//   - fn panics             → rollback, re-panic with the original value
//   - fn returns nil        → commit, return the commit's error
//
// fn's error wins over the rollback's on purpose. The rollback error is nearly
// always a consequence of the first failure — a broken connection reports both
// — and returning it instead would replace the cause with the symptom at the
// one moment a caller is reading the message to find out what happened.
//
// There is no retry loop here and no attempt budget. Retrying is real —
// Retryable(err) is true for a serialization failure and a deadlock, which
// both mean "run the whole transaction again" — but how many times, how long
// to wait, and whether the work is safe to repeat are the caller's to answer,
// and a library that picked for them would be wrong in the one deployment that
// mattered. Written out, the loop is four lines:
//
//	for range 3 {
//	    err = storm.InTx(ctx, db, func(ex storm.Executor) error { ... })
//	    if !storm.Retryable(err) { break }
//	}
func InTx(ctx context.Context, db DB, fn func(ex Executor) error) (err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}

	// Named-result recover rather than a bool flag: a panicking fn must not
	// leave the transaction open and its connection checked out, and it must
	// not have its panic value swallowed either.
	committed := false
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}

	// Set before Commit, not after. A commit that fails has still ENDED the
	// transaction, so the deferred rollback would be a second end on a
	// finished Tx — harmless per the Rollback contract, but it would also be
	// a second round trip on a connection that just told us it has a problem.
	committed = true
	return tx.Commit(ctx)
}
