// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package aliasrowx deliberately declares a name that differs from its
// directory (aliasrow). Real adopters do this — anubis keeps package
// authzrquery in a directory named rquery — and the raw-scanner emitter must
// qualify row types by the DECLARED name while importing the directory path.
package aliasrowx

import "github.com/gsoultan/storm/runtime"

// Row is a raw-query row type for codegen's alias regression test. Note is
// Null[T] because reflect names a generic instantiation "Null[string]", not
// "Null" — the shape detection must match the prefix, and anubis's first
// nullable raw column is what proved it didn't.
type Row struct {
	Name string
	Note runtime.Null[string]
}
