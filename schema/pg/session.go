// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package schemapg

import (
	"context"
	"fmt"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/schema"
)

// Session is a connection of any driver that introspection reads through,
// named without naming the driver. storm's caduceus adapter gives one.
type Session interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	Exec(ctx context.Context, sql string, args ...any) error
}

// query runs one catalog read.
type query func(ctx context.Context, sql string, args ...any) (Rows, error)

// scopeSession is setSearchPath through a Session.
func scopeSession(ctx context.Context, c Session, ns string) (func() error, error) {
	if err := ValidNamespace(ns); err != nil {
		return nil, err
	}
	prev, err := showSearchPath(ctx, c)
	if err != nil {
		return nil, err
	}
	if err := c.Exec(ctx, "SET search_path TO "+ns); err != nil {
		return nil, fmt.Errorf("set search_path to %s: %w", ns, err)
	}
	return func() error {
		if err := c.Exec(ctx, "SET search_path TO "+prev); err != nil {
			return fmt.Errorf("restore search_path: %w", err)
		}
		return nil
	}, nil
}

// showSearchPath reads the connection's search_path.
func showSearchPath(ctx context.Context, c Session) (string, error) {
	rows, err := c.Query(ctx, "SHOW search_path")
	if err != nil {
		return "", fmt.Errorf("read search_path: %w", err)
	}
	defer rows.Close()
	var prev string
	if rows.Next() {
		if err := rows.Scan(&prev); err != nil {
			return "", fmt.Errorf("read search_path: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read search_path: %w", err)
	}
	return prev, nil
}

// IntrospectCaduceus is Introspect on a caduceus connection.
func IntrospectCaduceus(ctx context.Context, c *caduceus.Conn, namespace string) (*schema.Schema, error) {
	return IntrospectSession(ctx, caduceusSession{c}, namespace)
}

// caduceusSession is a caduceus connection as a Session.
type caduceusSession struct{ c *caduceus.Conn }

func (s caduceusSession) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	r, err := s.c.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (s caduceusSession) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := s.c.Exec(ctx, sql, args...)
	return err
}
