// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgdrv

import (
	"fmt"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/caduceus/wire"
)

// rows adapts caduceus's rows to runtime.Rows.
//
// done, when set, runs once when the rows are closed: a pool's rows hold
// their connection until then, because they stream off it.
type rows struct {
	r    *caduceus.Rows
	done func()
}

func (r *rows) Next() bool          { return r.r.Next() }
func (r *rows) RawValues() [][]byte { return r.r.RawValues() }

func (r *rows) Close() {
	r.r.Close()
	if r.done != nil {
		done := r.done
		r.done = nil
		done()
	}
}

// Err classifies too. A statement's error can arrive with its rows rather than
// with the call that sent it: a constraint violation on a RETURNING insert is
// reported once the rows are read, which is the path constraints are on.
func (r *rows) Err() error { return classify(r.r.Err()) }

// Generated scanners decode the RAW wire bytes, which means storm has a wire
// FORMAT requirement. PostgreSQL can send every column as binary or as text,
// and for most types the two are nothing alike: `false` is one zero byte in
// binary and the byte 'f' in text, so runtime.Bool reads a text false as TRUE,
// and an int8 in text panics a decoder that expects eight bytes.
//
// Every connection this package opens asks for storm's types in binary, so the
// check below holds on all of them. It is kept, once per result, because a
// caller can hand Conn a caduceus connection of their own, and one opened
// without storm's options asks for numeric, inet, cidr and tstzrange as text.

// formatOK reports whether storm can decode a column of type oid in format.
//
// It names the types that text BREAKS rather than the ones it does not,
// because the safe set is open-ended: text, varchar and name are the same bytes
// either way, jsonb's binary form is its text behind one version byte that
// runtime.JSONB strips, and an enum's label is the value storm scans into a
// string. Domains report their base type's OID, so they get no free pass.
func formatOK(oid uint32, format int16) bool {
	if format == wire.BinaryFormat {
		return true
	}
	switch oid {
	case 16, // bool — 'f' is 0x66, non-zero, so text false reads as TRUE
		21, 23, 20, // int2, int4, int8
		700, 701, // float4, float8
		17,               // bytea — text is the \x hex escape, not the bytes
		2950,             // uuid — text is 36 dashed characters, not 16 bytes
		1184, 1082, 1186, // timestamptz, date, interval
		1083,     // time
		1700,     // numeric
		869, 650, // inet, cidr
		3910,       // tstzrange
		1009, 1015, // text[], varchar[]
		2951, // uuid[]
		1016, // int8[]
		1231: // numeric[]
		return false
	}
	return true
}

// newRows wraps a result after checking it can be decoded. On refusal the
// result is closed here, done included: the caller gets an error and no Rows,
// so there is nothing left for them to close.
func newRows(r *caduceus.Rows, done func()) (*rows, error) {
	for i, f := range r.Fields() {
		if formatOK(f.DataTypeOID, f.Format) {
			continue
		}
		r.Close()
		if done != nil {
			done()
		}
		return nil, fmt.Errorf(
			"storm: column %d %q (oid %d) arrived in text format and storm decodes binary — "+
				"this connection was opened without storm's options. Open it with pgdrv.Connect, "+
				"which asks for storm's types in binary; see docs/DEPLOYMENT.md",
			i+1, f.Name, f.DataTypeOID)
	}
	return &rows{r: r, done: done}, nil
}
