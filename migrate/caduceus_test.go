// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package migrate_test

import (
	"testing"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/internal/testmodel"
	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/runtime/caduceusdrv"
	"github.com/gsoultan/storm/schema/pg"
)

// Automigrate over caduceus builds the same schema it builds over pgx: a
// second run has nothing to do, and introspecting the result through either
// driver reads the same schema.
func TestAutoCaduceusMatchesPgx(t *testing.T) {
	const ns = "storm_auto_caduceus"
	ctx, pc := conn(t, ns)
	want, err := storm.Build(testmodel.All()...)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := caduceusdrv.NewPool(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	o := migrate.AutoOptions{Schema: ns}
	res, err := migrate.AutoCaduceusPool(ctx, pool, want, o)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res.Empty() {
		t.Fatal("the first run applied nothing")
	}
	again, err := migrate.AutoCaduceusPool(ctx, pool, want, o)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !again.Empty() {
		t.Fatalf("the second run applied %d step(s):\n%s", len(again.Applied), again.SQL())
	}
	err = pool.Acquire(ctx, func(c *caduceus.Conn) error {
		plan, err := migrate.ForCaduceus(ctx, c, ns, want, migrate.Options{})
		if err == nil && !plan.Empty() {
			t.Errorf("ForCaduceus after migrating plans %d step(s):\n%s", len(plan.Changes), plan.SQL())
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	viaPgx, err := migrate.Auto(ctx, pc, want, o)
	if err != nil || !viaPgx.Empty() {
		t.Fatalf("pgx after caduceus: %v, %d step(s):\n%s", err, len(viaPgx.Applied), viaPgx.SQL())
	}

	fromPgx, err := schemapg.Introspect(ctx, pc, ns)
	if err != nil {
		t.Fatal(err)
	}
	var fromCaduceus = fromPgx
	err = pool.Acquire(ctx, func(c *caduceus.Conn) error {
		fromCaduceus, err = schemapg.IntrospectCaduceus(ctx, c, ns)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := migrate.Diff(fromPgx, fromCaduceus); !d.Empty() {
		t.Fatalf("the two drivers read different schemas:\n%s", d.SQL())
	}
	if d := migrate.Diff(fromCaduceus, fromPgx); !d.Empty() {
		t.Fatalf("the two drivers read different schemas:\n%s", d.SQL())
	}
}
