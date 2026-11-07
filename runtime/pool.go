// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package runtime

// Pool is a typed sync.Pool. Generated code needs one per table, and the
// generic wrapper keeps the type assertion out of the generated source.
type Pool[T any] struct {
	p   chan *T
	new func() *T
}

// NewPool builds a pool. The channel is bounded, so a burst cannot pin memory.
func NewPool[T any](new func() *T) *Pool[T] {
	return &Pool[T]{p: make(chan *T, 64), new: new}
}

func (p *Pool[T]) Get() *T {
	select {
	case v := <-p.p:
		return v
	default:
		return p.new()
	}
}

func (p *Pool[T]) Put(v *T) {
	select {
	case p.p <- v:
	default:
	}
}
