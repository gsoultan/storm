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
	"github.com/gsoultan/storm/v2/runtime/caduceusdrv"
	"github.com/gsoultan/storm/v2/schema"
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
func AutoPool(ctx context.Context, p *caduceusdrv.Pool, want *schema.Schema, o AutoOptions) (Result, error) {
	var res Result
	var ran bool
	err := p.Acquire(ctx, func(c *caduceus.Conn) error {
		ran = true
		var err error
		res, err = Auto(ctx, c, want, o)
		return err
	})
	if err != nil && !ran {
		return Result{}, fmt.Errorf("acquire a connection to migrate on: %w", err)
	}
	return res, err
}
