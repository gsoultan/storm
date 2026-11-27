// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgdrv_test

import (
	"context"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgdrv"
)

// Each of storm's value types, sent through the adapter and read back as the
// server's own text, which is the only judge of what the bytes meant.
func TestStormValueTypesBind(t *testing.T) {
	ctx := context.Background()
	pool, err := pgdrv.NewPool(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	tod, _ := runtime.NewTimeOfDay(13, 45, 7, 250)
	from := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	dec, _ := runtime.ParseDecimal("-12.50")
	for _, c := range []struct {
		sql  string
		arg  any
		want string
	}{
		{"SELECT $1::numeric::text", dec, "-12.50"},
		{"SELECT $1::numeric IS NULL", (*runtime.Decimal)(nil), "t"},
		{"SELECT $1::interval::text", runtime.Interval{Months: 14, Days: 3, Micros: 3_600_000_000}, "1 year 2 mons 3 days 01:00:00"},
		{"SELECT $1::time::text", tod, "13:45:07.00025"},
		{"SELECT $1::time IS NULL", (*runtime.TimeOfDay)(nil), "t"},
		{"SELECT lower($1::tstzrange) = '2026-01-02 03:04:05+00'", runtime.Since(from), "t"},
		{"SELECT ($1::numeric[])[2]::text", []runtime.Decimal{dec, dec}, "-12.50"},
		{"SELECT $1::jsonb ->> 'a'", runtime.JSON(`{"a":"b"}`), "b"},
		{"SELECT array_length($1::int8[], 1)::text", []int64{1, 2, 3}, "3"},
		{"SELECT ($1::text[])[2]", []string{"a", "b,c"}, "b,c"},
		{"SELECT ($1::uuid[])[1]::text", [][16]byte{{0xde, 0xad}}, "dead0000-0000-0000-0000-000000000000"},
	} {
		rows, err := pool.Query(ctx, c.sql, []any{c.arg})
		if err != nil {
			t.Errorf("%s with %T: %v", c.sql, c.arg, err)
			continue
		}
		if !rows.Next() {
			t.Errorf("%s: no row: %v", c.sql, rows.Err())
			rows.Close()
			continue
		}
		got := rows.RawValues()[0]
		s := string(got)
		if len(got) == 1 && (got[0] == 0 || got[0] == 1) { // a binary bool
			s = map[byte]string{0: "f", 1: "t"}[got[0]]
		}
		rows.Close()
		if s != c.want {
			t.Errorf("%s with %T = %q, want %q", c.sql, c.arg, s, c.want)
		}
	}
}
