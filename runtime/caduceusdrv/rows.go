// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package caduceusdrv

import (
	"fmt"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/runtime"
)

// rows adapts caduceus's rows to runtime.Rows. release, when set, gives the
// pooled connection back once the rows are closed, and runs once.
type rows struct {
	r       *caduceus.Rows
	release func()
}

var _ runtime.Rows = (*rows)(nil)

func (r *rows) Next() bool          { return r.r.Next() }
func (r *rows) RawValues() [][]byte { return r.r.RawValues() }

// Err classifies too: a constraint broken by an INSERT ... RETURNING is
// reported when its rows are read, not when the statement is sent.
func (r *rows) Err() error { return classify(r.r.Err()) }

func (r *rows) Close() {
	r.r.Close()
	if r.release != nil {
		release := r.release
		r.release = nil
		release()
	}
}

// newRows checks the result arrives in the formats storm decodes and wraps
// it. A connection opened without Options asks for numeric, jsonb, inet and
// cidr in text, which the generated scanners would misread rather than
// refuse, so such a result is refused here, naming the column.
func newRows(r *caduceus.Rows, release func()) (*rows, error) {
	for i, f := range r.Fields() {
		if f.Format == ResultFormat(f.DataTypeOID) || sameInBoth(f.DataTypeOID) {
			continue
		}
		{
			r.Close()
			if release != nil {
				release()
			}
			return nil, fmt.Errorf(
				"storm: column %d %q (oid %d) arrived in format %d and storm decodes format %d — "+
					"open the connection with caduceusdrv.Options()", i+1, f.Name, f.DataTypeOID, f.Format, ResultFormat(f.DataTypeOID))
		}
	}
	return &rows{r: r, release: release}, nil
}

// sameInBoth reports whether a type's text and binary forms are the same
// bytes, so a column of it reads right in either.
func sameInBoth(oid uint32) bool {
	switch oid {
	case 25, 1043, 1042, 19, 114, 142, 705: // text, varchar, bpchar, name, json, xml, unknown
		return true
	}
	return false
}
