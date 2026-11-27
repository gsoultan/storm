// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package caduceusdrv is storm's adapter for caduceus
// (github.com/gsoultan/caduceus), a PostgreSQL driver with no dependencies.
// It is the only package in runtime that knows caduceus exists, and no
// caduceus type crosses out of it but the connection a caller hands in.
//
// Pool is the usual way in: a pool of connections that is a runtime.DB, so
// every generated package takes it unchanged. Conn adapts one connection the
// caller holds, and Tx a transaction on either.
//
// storm's generated scanners decode PostgreSQL's binary format, and caduceus
// asks for numeric, jsonb, inet and cidr in text unless told otherwise. Every
// connection this package opens is told, through Options; a connection the
// caller opens for Conn must be opened with Options too, or those columns
// are refused when they arrive (see checkFormats).
package caduceusdrv

import (
	"context"

	"github.com/gsoultan/caduceus"
)

// Options returns the caduceus options storm's scanners need: every result
// column in the format pgx would ask for it in, which is binary for each
// built-in type pgx can decode and text for the rest, such as an enum.
func Options() caduceus.Options {
	return caduceus.Options{ResultFormat: ResultFormat}
}

// ResultFormat is the format, 1 for binary and 0 for text, a result column
// of type oid is asked for in. It matches pgx's default type map, so a
// generated package decodes the same bytes over either adapter.
func ResultFormat(oid uint32) int16 {
	if binaryOIDs[oid] {
		return 1
	}
	return 0
}

// binaryOIDs are the built-in types pgx decodes in binary, and their arrays.
// aclitem and jsonpath are text-only in pgx, and pgx reads jsonb as text, so
// those are left out, as is every
// type the server made after initdb (an enum, a domain's base aside, an
// extension's type), whose binary form storm cannot know.
var binaryOIDs = map[uint32]bool{}

func init() {
	for _, oid := range []uint32{
		16, 17, 18, 19, 20, 21, 23, 25, 26, 27, 28, 29, 114, 142, 600, 601,
		602, 603, 604, 628, 650, 700, 701, 705, 718, 774, 829, 869, 1042, 1043,
		1082, 1083, 1114, 1184, 1186, 1560, 1562, 1700, 2249, 2950, 3614,
		3904, 3906, 3908, 3910, 3912, 3926, 4451, 4532, 4533, 4534, 4535, 4536, 5069,
		// Their arrays.
		1000, 1001, 1002, 1003, 1016, 1005, 1007, 1009, 1028, 1010, 1011, 1012,
		199, 143, 1017, 1018, 1019, 1020, 1027, 629, 651, 1021, 1022, 719, 775,
		1040, 1041, 1014, 1015, 1182, 1183, 1115, 1185, 1187, 1561, 1563, 1231,
		2287, 2951, 3643, 3905, 3907, 3909, 3911, 3913, 3927, 6150, 6151,
		6152, 6153, 6155, 6157, 271,
	} {
		binaryOIDs[oid] = true
	}
}

// Connect opens one connection with the Options storm needs.
func Connect(ctx context.Context, dsn string) (*caduceus.Conn, error) {
	return caduceus.Connect(ctx, dsn, Options())
}
