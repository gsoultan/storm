package runtime

import "context"

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
//
// The method is StartTx rather than Begin, and the name is a compatibility
// decision, not a matter of taste. mydrv's and msdrv's pools shipped in v1.1.0
// with Begin(ctx) (*Tx, error), a concrete return type, and Go has no covariant
// returns: a DB whose method was also called Begin could only be satisfied by
// changing theirs, which docs/STABILITY.md forbids in a minor and
// scripts/check/apicompat.sh rejects. It is not BeginTx either, because inside
// runtime/sqldrv that would sit beside database/sql's BeginTx(ctx, *TxOptions),
// a different method with the same name one layer down.
type DB interface {
	Executor

	// StartTx starts a transaction. The returned Tx is an Executor, so every
	// generated surface takes it unchanged.
	StartTx(ctx context.Context) (Tx, error)
}
