// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package pgdrv is storm's PostgreSQL adapter, on the caduceus driver
// (github.com/gsoultan/caduceus). It is the only package in storm that knows
// caduceus exists, and no caduceus type crosses out of it except where an
// adopter asks for a connection by name (AGENTS.md, CI-enforced).
//
//	pool, err := pgdrv.NewPool(ctx, "postgres://app:secret@db/app")
//	defer pool.Close()
//	rows, err := user.New().Where(user.Email.Eq(e)).All(ctx, pool, nil)
//
// Pool, Tx and Conn are the three Executors: a pool of connections, a
// transaction on one of them, and one connection pinned for state that lives
// on a session, such as an advisory lock.
package pgdrv

import (
	"context"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/caduceus/codec"
	"github.com/gsoultan/caduceus/wire"
)

// resultFormat asks for every column storm's generated scanners decode in
// binary. caduceus asks for numeric, inet, cidr, tstzrange and numeric[] in
// text by default, which storm's scanners refuse, so every connection this
// package opens passes this, and Options cannot take it away. Everything else
// is in the format caduceus would choose.
func resultFormat(oid uint32) int16 {
	switch oid {
	case codec.Numeric, codec.Inet, codec.CIDR,
		3910, // tstzrange
		1231: // numeric[]
		return 1
	}
	return codec.ResultFormat(oid)
}

// Options tunes the connections this package opens.
type Options struct {
	// StatementCacheCapacity is caduceus's: how many prepared statements each
	// connection keeps on the server. Zero is caduceus's default; negative
	// keeps none, for PgBouncer in transaction pooling mode.
	StatementCacheCapacity int

	// Tracer, if set, is told of every statement; see caduceus.QueryTracer.
	Tracer caduceus.QueryTracer
}

// caduceus is the caduceus options for o, with storm's binary result types.
func (o Options) caduceus() caduceus.Options {
	return caduceus.Options{
		StatementCacheCapacity: o.StatementCacheCapacity,
		Tracer:                 o.Tracer,
		ResultFormat:           resultFormat,
	}
}

// Connect opens one connection that storm's generated code can read from.
//
// A connection opened with caduceus.Connect directly asks for numeric, inet,
// cidr and tstzrange as text, which storm's scanners refuse; this is the one
// to use for Conn, and the one schema introspection and migrate.Auto take.
func Connect(ctx context.Context, dsn string, o Options) (*caduceus.Conn, error) {
	cfg, err := wire.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return caduceus.ConnectConfig(ctx, cfg, o.caduceus())
}

// session is what a connection and a transaction on one have in common: the
// four calls the Executor port is made of.
type session interface {
	Query(ctx context.Context, sql string, args ...any) (*caduceus.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (caduceus.CommandTag, error)
	CopyFrom(ctx context.Context, table, columns []string, rows caduceus.CopySource) (int64, error)
	SendBatch(ctx context.Context, queries []caduceus.Query) (*caduceus.BatchResults, error)
}

var (
	_ session = (*caduceus.Conn)(nil)
	_ session = (*caduceus.Tx)(nil)
)
