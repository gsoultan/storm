// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package caduceusdrv

import "github.com/gsoultan/caduceus"

// Row is the result of Pool.QueryRow.
type Row struct {
	r       *caduceus.Row
	release ender
	err     error
}

// Scan reads the row into dest, as caduceus's Row.Scan does, and gives the
// connection back. A query that returned no row is caduceus.ErrNoRows.
func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	err := r.r.Scan(dest...)
	r.release.end()
	return classify(err)
}
