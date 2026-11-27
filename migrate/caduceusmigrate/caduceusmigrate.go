// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package caduceusmigrate runs storm's automigrate and PostgreSQL
// introspection over caduceus. It is a package of its own so that a project
// on pgx, and the storm command, never link caduceus.
package caduceusmigrate

import (
	"context"
	"fmt"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/runtime/caduceusdrv"
	"github.com/gsoultan/storm/schema"
)

// Auto is migrate.Auto on a caduceus connection.
func Auto(ctx context.Context, c *caduceus.Conn, want *schema.Schema, o migrate.AutoOptions) (migrate.Result, error) {
	return migrate.AutoSession(ctx, Session(c), want, o)
}

// AutoPool is migrate.AutoPool on storm's caduceus pool: one connection is
// held for the whole migration, because the advisory lock that serialises
// migrating processes belongs to a session.
func AutoPool(ctx context.Context, p *caduceusdrv.Pool, want *schema.Schema, o migrate.AutoOptions) (res migrate.Result, err error) {
	aerr := p.Acquire(ctx, func(c *caduceus.Conn) error {
		res, err = Auto(ctx, c, want, o)
		return nil
	})
	if aerr != nil {
		return migrate.Result{}, fmt.Errorf("acquire a connection to migrate on: %w", aerr)
	}
	return res, err
}

// For is migrate.ForWith on a caduceus connection: the plan that would bring
// target up to want, applied nowhere.
func For(ctx context.Context, c *caduceus.Conn, target string, want *schema.Schema, o migrate.Options) (migrate.Plan, error) {
	return migrate.ForSession(ctx, Session(c), target, want, o)
}

// Session is a caduceus connection as a migrate.Session.
func Session(c *caduceus.Conn) migrate.Session {
	s := statements(c.QueryRow, c.Exec)
	s.Begin = func(ctx context.Context) (migrate.Session, error) {
		tx, err := c.Begin(ctx)
		if err != nil {
			return migrate.Session{}, err
		}
		t := statements(tx.QueryRow, tx.Exec)
		t.Commit, t.Rollback = tx.Commit, tx.Rollback
		return t, nil
	}
	s.Introspect = func(ctx context.Context, ns string) (*schema.Schema, error) {
		return Introspect(ctx, c, ns)
	}
	return s
}

// statements runs statements through a caduceus connection's or
// transaction's QueryRow and Exec.
func statements(
	queryRow func(ctx context.Context, sql string, args ...any) *caduceus.Row,
	run func(ctx context.Context, sql string, args ...any) (caduceus.CommandTag, error),
) migrate.Session {
	return migrate.Session{
		Scan: func(ctx context.Context, dest any, sql string, args ...any) error {
			return queryRow(ctx, sql, args...).Scan(dest)
		},
		Exec: func(ctx context.Context, sql string, args ...any) error {
			_, err := run(ctx, sql, args...)
			return err
		},
	}
}
