// Package runtime is what generated code calls at query time.
//
// It is the only part of raorm on the hot path, and it obeys the rules in
// AGENTS.md: no reflect, no `any` boxing per column, no map lookup per query,
// no allocation on a warm path.
package runtime

import "sync/atomic"

// Op is a comparison operator's compiler-assigned id. Generated code numbers
// the argument-taking operators first, so `op-1 < opsWithArg` is one unsigned
// compare on the bind path.
//
// Ids only. An Op never carries a caller's value, which is why two queries
// with the same structure and different values share one statement.
type Op uint32

// Stmt is a compiled statement: immutable, interned, shared by every caller
// that produces the same shape.
type Stmt struct {
	SQL  string
	NArg int

	// slabHint is the byte size the last result of this shape needed from a
	// Slab. Shapes are stable, so one observation sizes the next arena exactly
	// and the doubling ramp never runs. Measured worth 1.8x throughput.
	slabHint atomic.Int64
}

// SlabHint reports the size to pre-size an arena to.
func (s *Stmt) SlabHint() int { return int(s.slabHint.Load()) }

// ObserveSlab records how much arena the last result actually used.
func (s *Stmt) ObserveSlab(n int) { s.slabHint.Store(int64(n)) }

// Frag is one lowered predicate, split where its placeholder goes: A, then the
// placeholder, then B. The text is chosen by the back end in compile/ at
// generate time; runtime only splices.
type Frag struct{ A, B string }
