// Package runtime is what generated code calls at query time.
//
// It is the only part of raorm on the hot path, and it obeys the rules in
// AGENTS.md: no reflect, no `any` boxing per column, no map lookup per query,
// no allocation on a warm path.
package runtime

import (
	"errors"
	"sync/atomic"
)

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

	// Err is set when the token stream that produced this statement was
	// malformed — a generator bug, not a caller one.
	//
	// It exists because the alternative is worse than an error. A stream whose
	// predicates do not reduce to a single expression used to have its WHERE
	// clause silently dropped, which turns "find the rows matching this" into
	// "find every row" — a filter that fails OPEN. Found by fuzzing, not by
	// review, because no generator emits such a stream today and none of the
	// hand-written tests could.
	//
	// Every terminal returns it before executing anything.
	Err error

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

// ErrMalformedStream is what Stmt.Err carries. Reaching it means generated code
// emitted a token stream whose predicates do not reduce to one expression:
// unbalanced arity on a group, or a column token no comparison consumed.
var ErrMalformedStream = errors.New(
	"raorm: generated token stream is malformed — its predicates do not reduce to a " +
		"single expression; this is a code-generation bug, please report it")
