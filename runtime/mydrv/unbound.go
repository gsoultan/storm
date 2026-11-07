// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mydrv

// unboundError is an argument with no wire form. Its own type because it is
// the one statement failure found BEFORE anything is written, so the
// connection is untouched — the pool keeps it, where an I/O failure discards
// it (see Conn.settle).
type unboundError struct{ err error }

func (e *unboundError) Error() string { return e.err.Error() }
