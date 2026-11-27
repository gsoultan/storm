// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package caduceusmigrate

import (
	"context"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/schema"
	"github.com/gsoultan/storm/schema/pg"
)

// Introspect is schemapg.Introspect on a caduceus connection.
func Introspect(ctx context.Context, c *caduceus.Conn, namespace string) (*schema.Schema, error) {
	return schemapg.IntrospectSession(ctx, introspectSession{c}, namespace)
}

// introspectSession is a caduceus connection as a schemapg.Session.
type introspectSession struct{ c *caduceus.Conn }

func (s introspectSession) Query(ctx context.Context, sql string, args ...any) (schemapg.Rows, error) {
	r, err := s.c.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (s introspectSession) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := s.c.Exec(ctx, sql, args...)
	return err
}
