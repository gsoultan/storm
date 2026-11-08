// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package runtime is what generated code calls at query time.
//
// It is the only part of storm on the hot path, and it obeys the rules in
// AGENTS.md: no reflect, no `any` boxing per column, no map lookup per query,
// no allocation on a warm path.
package runtime

import (
	"errors"
	"sync/atomic"
)

// Op is a comparison operator's id: OpEq and the rest, in tree.go. The
// argless operators are numbered last, so ArglessOp is one compare on the
// bind path.
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

	// hint is what the last result of this shape needed, packed so one load
	// reads a consistent pair: the row count in the high 32 bits (0 when the
	// reader did not count, as Each does not) and the arena bytes in the low
	// 32. Shapes are stable, so one observation sizes the next arena and
	// result slice exactly and neither doubling ramp runs. The arena half was
	// measured worth 1.8x throughput.
	hint atomic.Uint64
}

// SlabHint reports the size to pre-size an arena to.
func (s *Stmt) SlabHint() int { return int(uint32(s.hint.Load())) }

// ObserveSlab records how much arena the last result actually used, without a
// row count: ReadHint then sizes the arena unscaled and the slice not at all.
func (s *Stmt) ObserveSlab(n int) { s.hint.Store(uint64(clampHint(n))) }

// ObserveRead records the rows and arena bytes the last result of this shape
// actually used, for ReadHint to size the next one from.
func (s *Stmt) ObserveRead(rows, bytes int) {
	s.hint.Store(uint64(clampHint(rows))<<32 | uint64(clampHint(bytes)))
}

// ReadHint reports how many rows and how many arena bytes to pre-size for a
// read of this shape capped at limit rows.
//
// The statement is shared by every limit, because LIMIT is a bound argument,
// so the last observation can be a 1,000-row export when this call is a
// 20-row page. Both halves are scaled down to the limit — the bytes by the
// observed bytes per row — so a small read never inherits a large one's
// buffers. A limit at or below zero, or no row count, leaves them unscaled.
func (s *Stmt) ReadHint(limit int64) (rows, bytes int) {
	h := s.hint.Load()
	r, b := int64(h>>32), int64(uint32(h))
	if r > 0 && limit > 0 && limit < r {
		b = (b*limit + r - 1) / r
		r = limit
	}
	return int(r), int(b)
}

// clampHint keeps an observation inside its 32-bit half. A hint is a size to
// reserve, so saturating loses nothing a caller could use.
func clampHint(n int) uint32 {
	if n <= 0 {
		return 0
	}
	if uint64(n) > 1<<32-1 {
		return 1<<32 - 1
	}
	return uint32(n)
}

// Grow returns dst with room for at least n more elements, so a result whose
// size the statement has already seen is decoded into one allocation instead
// of a doubling ramp that copies every row it has read so far.
func Grow[T any](dst []T, n int) []T {
	if n <= cap(dst)-len(dst) {
		return dst
	}
	out := make([]T, len(dst), len(dst)+n)
	copy(out, dst)
	return out
}

// Frag is one lowered predicate, split where its placeholder goes: A, then the
// placeholder, then B. The text is chosen by the back end in compile/ at
// generate time; runtime only splices.
type Frag struct{ A, B string }

// ErrMalformedStream is what Stmt.Err carries. Reaching it means generated code
// emitted a token stream whose predicates do not reduce to one expression:
// unbalanced arity on a group, or a column token no comparison consumed.
var ErrMalformedStream = errors.New(
	"storm: generated token stream is malformed — its predicates do not reduce to a " +
		"single expression; this is a code-generation bug, please report it")
