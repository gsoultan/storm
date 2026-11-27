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

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/runtime/caduceusdrv"
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

// AutoCaduceus is Auto on a caduceus connection.
func AutoCaduceus(ctx context.Context, c *caduceus.Conn, want *schema.Schema, o AutoOptions) (Result, error) {
	return auto(ctx, caduceusSession(c), want, o)
}

// AutoCaduceusPool is AutoPool on storm's caduceus pool: one connection is
// held for the whole migration, for the reason AutoPool gives.
func AutoCaduceusPool(ctx context.Context, p *caduceusdrv.Pool, want *schema.Schema, o AutoOptions) (res Result, err error) {
	aerr := p.Acquire(ctx, func(c *caduceus.Conn) error {
		res, err = AutoCaduceus(ctx, c, want, o)
		return nil
	})
	if aerr != nil {
		return Result{}, fmt.Errorf("acquire a connection to migrate on: %w", aerr)
	}
	return res, err
}

// ForCaduceus is ForWith on a caduceus connection: the plan that would bring
// target up to want, applied nowhere.
func ForCaduceus(ctx context.Context, c *caduceus.Conn, target string, want *schema.Schema, o Options) (Plan, error) {
	return forWith(ctx, caduceusSession(c), target, want, o)
}

// session is the connection a migration runs on, whatever its driver: what
// Auto, Normalize and ForWith need of it and nothing more.
type session struct {
	// scan runs a query whose one row has one column, into dest.
	scan func(ctx context.Context, dest any, sql string, args ...any) error
	exec execer
	// begin starts a transaction, given as a session of its own whose
	// commit and rollback end it.
	begin      func(ctx context.Context) (session, error)
	commit     func(ctx context.Context) error
	rollback   func(ctx context.Context) error
	introspect func(ctx context.Context, ns string) (*schema.Schema, error)
}

func pgxSession(c *pgx.Conn) session {
	s := pgxStatements(c.QueryRow, c.Exec)
	s.begin = func(ctx context.Context) (session, error) {
		tx, err := c.Begin(ctx)
		if err != nil {
			return session{}, err
		}
		t := pgxStatements(tx.QueryRow, tx.Exec)
		t.commit, t.rollback = tx.Commit, tx.Rollback
		return t, nil
	}
	s.introspect = func(ctx context.Context, ns string) (*schema.Schema, error) {
		return schemapg.Introspect(ctx, c, ns)
	}
	return s
}

// pgxStatements runs statements through a pgx connection's or
// transaction's QueryRow and Exec.
func pgxStatements(
	queryRow func(ctx context.Context, sql string, args ...any) pgx.Row,
	run func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error),
) session {
	return session{
		scan: func(ctx context.Context, dest any, sql string, args ...any) error {
			return queryRow(ctx, sql, args...).Scan(dest)
		},
		exec: func(ctx context.Context, sql string, args ...any) error {
			_, err := run(ctx, sql, args...)
			return err
		},
	}
}

func caduceusSession(c *caduceus.Conn) session {
	s := caduceusStatements(c.QueryRow, c.Exec)
	s.begin = func(ctx context.Context) (session, error) {
		tx, err := c.Begin(ctx)
		if err != nil {
			return session{}, err
		}
		t := caduceusStatements(tx.QueryRow, tx.Exec)
		t.commit, t.rollback = tx.Commit, tx.Rollback
		return t, nil
	}
	s.introspect = func(ctx context.Context, ns string) (*schema.Schema, error) {
		return schemapg.IntrospectCaduceus(ctx, c, ns)
	}
	return s
}

// caduceusStatements runs statements through a caduceus connection's or
// transaction's QueryRow and Exec.
func caduceusStatements(
	queryRow func(ctx context.Context, sql string, args ...any) *caduceus.Row,
	run func(ctx context.Context, sql string, args ...any) (caduceus.CommandTag, error),
) session {
	return session{
		scan: func(ctx context.Context, dest any, sql string, args ...any) error {
			return queryRow(ctx, sql, args...).Scan(dest)
		},
		exec: func(ctx context.Context, sql string, args ...any) error {
			_, err := run(ctx, sql, args...)
			return err
		},
	}
}
