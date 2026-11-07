// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package pgstat reads what a PostgreSQL server knows about its sessions:
// which one a connection is, what each is doing, which locks it holds or waits
// for, and who it waits behind.
//
// For tests and diagnostics about concurrency, which have to see one session
// waiting for another to know they made the interleaving they are about. The
// catalogs it reads are PostgreSQL's own; nothing here is generated from a
// model, and no other back end has them.
package pgstat
