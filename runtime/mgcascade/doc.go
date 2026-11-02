// Package mgcascade performs a MongoDB collection's referential actions —
// what a SQL database does on its own when a parent row is deleted, and a
// document store does not do at all.
//
// A generated package for a collection that other collections reference with
// ON DELETE CASCADE, SET NULL, SET DEFAULT or RESTRICT carries a Plan: the
// collections that point at it, the fields they point with, and what each
// one's declaration says to do. Its deletes run through Delete, which reads
// the referenced values of the documents about to go, checks every RESTRICT,
// cascades, nulls or defaults the children — recursively, through their own
// references — and only then deletes. The Plan is the whole transitive graph,
// written at generate time from the model, so a package never imports another
// to find out what references it.
//
// Atomicity is a transaction's: Delete opens one when it is handed a pool
// (runtime.DB) and joins the caller's when it is handed a runtime.Tx. Handed
// neither — a decorator — it runs the steps in order, RESTRICT checks first,
// and a failure part-way leaves what was done done; the generated comment
// says so.
//
// What is NOT enforced is the other half of a foreign key: a child inserted
// with a parent that does not exist, or concurrently with the parent's
// delete. A check before an insert would race the delete it was meant to
// stop — MongoDB has no lock a reader takes on a document it did not write —
// so storm does not pretend to one.
//
// No reflection and no formatted printing, as everywhere under runtime/.
package mgcascade

// Action is what a reference says to do when its parent is deleted.
type Action uint8

const (
	// Restrict refuses the delete while a child references the parent.
	Restrict Action = iota + 1
	// Cascade deletes the children, and theirs, as the parent goes.
	Cascade
	// SetNull and SetDefault rewrite the children's reference fields.
	SetNull
	SetDefault
)

// MaxDepth bounds a cascade's recursion: a cycle in the data — a reference
// graph that loops back on itself — would otherwise never end.
const MaxDepth = 64
