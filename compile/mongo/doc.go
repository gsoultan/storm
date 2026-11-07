// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package mongo lowers storm's logical plan to MongoDB commands.
//
// MongoDB is a back end, not a dialect (ADR-0004). The SQL back ends lower a
// plan to SQL text; this one lowers it to a COMMAND DOCUMENT, written as JSON
// with numbered holes — `{"$arg":1}` — where a caller's value goes. The text is
// the statement: it is what the shape cache keys, what a test asserts and what
// runtime/mgdrv compiles once and binds per call. Nothing at run time asks
// which back end produced it.
//
// Everything here runs at GENERATE time and returns strings, exactly as
// compile/pgsql does. A construct this package has no lowering for is absent
// rather than approximated, and codegen/mongo refuses it naming the construct.
package mongo
