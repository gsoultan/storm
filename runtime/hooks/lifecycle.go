package hooks

import (
	"context"

	"github.com/gsoultan/storm/runtime"
)

// Lifecycle is a table's lifecycle hooks: the type of the Hooks variable
// every generated table package with a primary key declares, instantiated
// with that package's Ins, Mut and Row.
//
//	func init() {
//	    article.Hooks.BeforeCreate = func(ctx context.Context, ex storm.Executor, n *article.Ins) error {
//	        n.SetTenantID(tenantFrom(ctx))
//	        return nil
//	    }
//	    article.Hooks.AfterDelete = func(ctx context.Context, ex storm.Executor, key *article.Row) error {
//	        return audit.Insert(ctx, ex, &audit.Row{Table: "articles", RowID: key.ID})
//	    }
//	}
//
// Every hook receives the Executor the call was given, so a hook that writes
// does so in the caller's transaction, and an error from any hook is returned
// to the caller as it is. An error from a Before hook stops the write before
// anything is sent. An error from an After hook is returned after the
// statement has run: inside a transaction (storm.InTx) that rolls it back,
// and outside one the write has already happened.
//
// Which calls run which hook:
//
//   - BeforeCreate, AfterCreate: Ins.Insert and Insert. Insert hands
//     BeforeCreate an Ins with every column assigned, and copies what the hook
//     set back into the caller's Row. AfterCreate gets the row as written.
//   - BeforeUpdate, AfterUpdate: Mut.Update.
//   - BeforeDelete, AfterDelete: Delete and, on a soft-delete table,
//     HardDelete. The Row holds only the primary key.
//   - AfterFind: every full-row read — All, AllInto, List, One, Each, Rows.
//     All and its relatives run it once the result is closed, so the hook may
//     use ex; Each and Rows run it for each row while the result is still
//     open, so on a transaction it must not send a statement through ex.
//
// Batches, bulk loads (InsertAll), set-based writes (UpdateAll, DeleteAll,
// DeleteBatch), upserts staged as batch ops, projections, plans and
// aggregates do not run lifecycle hooks: they write or read many rows in one
// statement and never hold one Row per row to hand over. Statement hooks
// (Wrap) see those.
//
// Set the fields during program start, before the first query. They are read
// without synchronisation on every call, which is what makes an unset hook
// cost one nil check.
type Lifecycle[I, M, R any] struct {
	BeforeCreate func(ctx context.Context, ex runtime.Executor, n *I) error
	AfterCreate  func(ctx context.Context, ex runtime.Executor, r *R) error

	BeforeUpdate func(ctx context.Context, ex runtime.Executor, m *M) error
	AfterUpdate  func(ctx context.Context, ex runtime.Executor, m *M) error

	BeforeDelete func(ctx context.Context, ex runtime.Executor, key *R) error
	AfterDelete  func(ctx context.Context, ex runtime.Executor, key *R) error

	AfterFind func(ctx context.Context, ex runtime.Executor, r *R) error
}

// Apply runs h on *p, if h is set: the hook a generated terminal calls.
//
// It hands h a pointer to a copy and writes the copy back, rather than
// handing over p. A pointer passed to a function value escapes, so passing p
// would move the caller's Ins, Mut or Row to the heap on every call, hook set
// or not; the copy is made only once a hook is found, so the cost lands on the
// calls that asked for it.
func Apply[T any](ctx context.Context, ex runtime.Executor, h func(context.Context, runtime.Executor, *T) error, p *T) error {
	if h == nil {
		return nil
	}
	c := *p
	err := h(ctx, ex, &c)
	*p = c
	return err
}
