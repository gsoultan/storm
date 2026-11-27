// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package migrate

import (
	"context"
	"fmt"

	"github.com/gsoultan/storm/schema"
	"github.com/gsoultan/storm/schema/pg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AutoPool is Auto against a pool, which is what an application actually holds.
//
// It acquires one connection and holds it for the whole migration. That is not
// an optimisation: the advisory lock serialising migrating processes is
// SESSION-scoped, so a migration spread across pooled connections would take
// the lock on one session and apply the DDL on another — which is to say, not
// hold it at all.
//
//	if _, err := migrate.AutoPool(ctx, pool, s, migrate.AutoOptions{
//		Logf: log.Printf,
//	}); err != nil {
//		return err
//	}
//
// where s came from storm.Build(model.All()...).
func AutoPool(ctx context.Context, p *pgxpool.Pool, want *schema.Schema, o AutoOptions) (Result, error) {
	c, err := p.Acquire(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("acquire a connection to migrate on: %w", err)
	}
	defer c.Release()
	return Auto(ctx, c.Conn(), want, o)
}

// AutoSession is Auto on a connection of a driver migrate does not import,
// given as a Session. storm's caduceus adapter, migrate/caduceusmigrate, is
// one.
func AutoSession(ctx context.Context, s Session, want *schema.Schema, o AutoOptions) (Result, error) {
	return auto(ctx, s, want, o)
}

// ForSession is ForWith on a Session: the plan that would bring target up to
// want, applied nowhere.
func ForSession(ctx context.Context, s Session, target string, want *schema.Schema, o Options) (Plan, error) {
	return forWith(ctx, s, target, want, o)
}

// Session is the connection a migration runs on, whatever its driver: what
// Auto, Normalize and ForWith need of it and nothing more. It must be one
// database session for the whole call, for the advisory lock's sake.
type Session struct {
	// Scan runs a query whose one row has one column, into dest.
	Scan func(ctx context.Context, dest any, sql string, args ...any) error
	// Exec runs a statement and discards its result.
	Exec func(ctx context.Context, sql string, args ...any) error
	// Begin starts a transaction, given as a Session of its own whose
	// Commit and Rollback end it. It needs only Scan, Exec, Commit and
	// Rollback.
	Begin    func(ctx context.Context) (Session, error)
	Commit   func(ctx context.Context) error
	Rollback func(ctx context.Context) error
	// Introspect reads namespace ns, as schemapg.Introspect does.
	Introspect func(ctx context.Context, ns string) (*schema.Schema, error)
}

func pgxSession(c *pgx.Conn) Session {
	s := pgxStatements(c.QueryRow, c.Exec)
	s.Begin = func(ctx context.Context) (Session, error) {
		tx, err := c.Begin(ctx)
		if err != nil {
			return Session{}, err
		}
		t := pgxStatements(tx.QueryRow, tx.Exec)
		t.Commit, t.Rollback = tx.Commit, tx.Rollback
		return t, nil
	}
	s.Introspect = func(ctx context.Context, ns string) (*schema.Schema, error) {
		return schemapg.Introspect(ctx, c, ns)
	}
	return s
}

// pgxStatements runs statements through a pgx connection's or
// transaction's QueryRow and Exec.
func pgxStatements(
	queryRow func(ctx context.Context, sql string, args ...any) pgx.Row,
	run func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error),
) Session {
	return Session{
		Scan: func(ctx context.Context, dest any, sql string, args ...any) error {
			return queryRow(ctx, sql, args...).Scan(dest)
		},
		Exec: func(ctx context.Context, sql string, args ...any) error {
			_, err := run(ctx, sql, args...)
			return err
		},
	}
}
