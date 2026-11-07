// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgdrv

// op is one step of a compiled command: write some fixed bytes, open or close
// a nested document, or bind an argument.
type op struct {
	kind uint8
	// raw is the fixed element bytes for opRaw, or the element header (type
	// byte and name) for opOpen.
	raw []byte
	// name is the element name an argument is bound under.
	name string
	// arg is the argument's index, from 0.
	arg int
}

const (
	opRaw uint8 = iota
	opOpen
	opClose
	opArg
)
