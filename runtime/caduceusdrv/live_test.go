// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package caduceusdrv_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/storm/v2/runtime"
	"github.com/gsoultan/storm/v2/runtime/caduceusdrv"
)

func dsn(t *testing.T) string {
	t.Helper()
	d := os.Getenv("STORM_DSN")
	if d == "" {
		t.Skip("STORM_DSN unset")
	}
	return d
}

func livePool(t *testing.T, opts ...string) *caduceusdrv.Pool {
	t.Helper()
	d := dsn(t)
	for _, o := range opts {
		if strings.Contains(d, "?") {
			d += "&" + o
		} else {
			d += "?" + o
		}
	}
	p, err := caduceusdrv.NewPool(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// scratch makes a table of its own for one test and drops it afterwards.
func scratch(t *testing.T, ex runtime.Executor, cols string) string {
	t.Helper()
	name := "cdrv_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := ex.Exec(t.Context(), "CREATE TABLE "+name+" ("+cols+")", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = ex.Exec(context.Background(), "DROP TABLE IF EXISTS "+name, nil) })
	return name
}

func rawRows(t *testing.T, ex runtime.Executor, sql string, args []any) [][][]byte {
	t.Helper()
	rows, err := ex.Query(t.Context(), sql, args)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer rows.Close()
	var out [][][]byte
	for rows.Next() {
		var row [][]byte
		for _, v := range rows.RawValues() {
			row = append(row, append([]byte(nil), v...))
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return out
}

// Values written through caduceusdrv, by INSERT and by COPY, hand storm's
// scanners the same bytes as the same values written by the server from a
// SQL literal, for every type storm decodes. The literal row is the server's
// own reading of the value, so no client codec is trusted on either side.
func TestSameBytesAsTheServersLiteral(t *testing.T) {
	ctx := t.Context()
	cp := livePool(t)

	tbl := scratch(t, cp, "id int, n numeric, iv interval, tod time, r tstzrange, ids uuid[], big int8[], words text[], ns numeric[], "+
		"doc jsonb, addr inet, net cidr, ok bool, blob bytea, at timestamptz, day date, u uuid, f float8, s smallint, label text")
	at := time.Date(2026, 11, 27, 8, 30, 15, 123456000, time.UTC)
	dec := runtime.Decimal{Unscaled: -123456789, Scale: 4}
	row := func(id int) []any {
		return []any{id, dec, runtime.Interval{Months: 14, Days: -3, Micros: 3_600_000_123},
			runtime.TimeOfDay(13*3600_000_000 + 5*60_000_000 + 7_000_001),
			runtime.NewTstzRange(at, at.Add(time.Hour)),
			[][16]byte{{1, 2, 3}, {4, 5, 6}}, []int64{1, -2, 1 << 40}, []string{"a", "b,c", ""},
			[]runtime.Decimal{{Unscaled: 15, Scale: 1}, {Unscaled: -7, Scale: 3}},
			runtime.JSON(`{"a": [1, 2]}`), "10.1.2.3", "10.0.0.0/8", true, []byte{0, 1, 255},
			at, at.Truncate(24 * time.Hour), [16]byte{9, 9}, 2.5, int16(-4), "hello"}
	}
	insert := "INSERT INTO " + tbl + " VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)"
	if _, err := cp.Exec(ctx, insert, row(1)); err != nil {
		t.Fatalf("insert through caduceus: %v", err)
	}
	literal := "INSERT INTO " + tbl + " VALUES (2, -12345.6789, '1 year 2 mons -3 days 01:00:00.000123', '13:05:07.000001', " +
		"'[\"2026-11-27 08:30:15.123456+00\",\"2026-11-27 09:30:15.123456+00\")', " +
		"'{01020300-0000-0000-0000-000000000000,04050600-0000-0000-0000-000000000000}', '{1,-2,1099511627776}', " +
		"'{a,\"b,c\",\"\"}', '{1.5,-0.007}', '{\"a\": [1, 2]}', '10.1.2.3', '10.0.0.0/8', true, '\\x0001ff', " +
		"'2026-11-27 08:30:15.123456+00', '2026-11-27', '09090000-0000-0000-0000-000000000000', 2.5, -4, 'hello')"
	if _, err := cp.Exec(ctx, literal, nil); err != nil {
		t.Fatalf("insert from a literal: %v", err)
	}
	// COPY, which a generated package's InsertAll uses, hands over pointers
	// to the row's fields.
	copied := func(id int) [][]any {
		vals := row(id)
		ptrs := make([]any, len(vals))
		for i := range vals {
			v := reflectPtr(vals[i])
			ptrs[i] = v
		}
		return [][]any{ptrs}
	}
	cols := []string{"id", "n", "iv", "tod", "r", "ids", "big", "words", "ns", "doc", "addr", "net", "ok", "blob", "at", "day", "u", "f", "s", "label"}
	if _, err := cp.CopyFrom(ctx, tbl, cols, &rowsSource{rows: copied(3)}); err != nil {
		t.Fatalf("copy through caduceus: %v", err)
	}
	all := "SELECT n, iv, tod, r, ids, big, words, ns, doc, addr, net, ok, blob, at, day, u, f, s, label FROM " + tbl + " WHERE id = $1"
	// Every row, however it was written, holds what the literal wrote.
	want := rawRows(t, cp, all, []any{2})
	if len(want) != 1 {
		t.Fatalf("the literal row: %d rows", len(want))
	}
	for _, id := range []int{1, 3} {
		got := rawRows(t, cp, all, []any{id})
		if len(got) != 1 {
			t.Fatalf("id %d: %d rows", id, len(got))
		}
		for i := range want[0] {
			if !bytes.Equal(got[0][i], want[0][i]) {
				t.Errorf("row %d column %d: %x, the literal wrote %x", id, i+1, got[0][i], want[0][i])
			}
		}
	}
	got := rawRows(t, cp, "SELECT n FROM "+tbl+" WHERE id = 1", nil)
	if d, err := runtime.DecodeNumeric(got[0][0]); err != nil || d.String() != dec.String() {
		t.Errorf("numeric read back = %v, %v; want %v", d, err, dec)
	}
}

func TestConstraintIsClassified(t *testing.T) {
	p := livePool(t)
	tbl := scratch(t, p, "id int PRIMARY KEY")
	ins := "INSERT INTO " + tbl + " VALUES ($1) RETURNING id"
	for i := 0; i < 2; i++ {
		rows, err := p.Query(t.Context(), ins, []any{1})
		if err == nil {
			for rows.Next() {
			}
			rows.Close()
			err = rows.Err()
		}
		if i == 1 && !errors.Is(err, runtime.ErrUniqueViolation) {
			t.Fatalf("second insert = %v, want ErrUniqueViolation", err)
		}
	}
	if _, err := p.Exec(t.Context(), "INSERT INTO "+tbl+" VALUES ($1)", []any{1}); !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Fatalf("Exec = %v, want ErrUniqueViolation", err)
	}
}

func TestTransactions(t *testing.T) {
	ctx := t.Context()
	p := livePool(t, "pool_max_conns=1")
	tbl := scratch(t, p, "id int")
	count := func() int {
		r := rawRows(t, p, "SELECT count(*)::int8 FROM "+tbl, nil)
		return int(int64(r[0][0][7]))
	}

	tx, err := p.StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO "+tbl+" VALUES (1)", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("after rollback: %d rows", n)
	}

	if err := runtime.InTx(ctx, p, func(tx runtime.Executor) error {
		_, err := tx.Exec(ctx, "INSERT INTO "+tbl+" VALUES (1)", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("after commit: %d rows", n)
	}

	tx, err = p.StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); !errors.Is(err, runtime.ErrTxDone) {
		t.Fatalf("second Commit = %v, want ErrTxDone", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback after Commit = %v, want nil", err)
	}
	// With one connection, the pool still answers: the transaction gave it back once.
	if n := count(); n != 1 {
		t.Fatalf("rows = %d", n)
	}
}

// A connection left mid-result is closed rather than handed to the next
// caller, and one holding an unsettled transaction is rolled back.
func TestReleaseLeavesNothingBehind(t *testing.T) {
	ctx := t.Context()
	p := livePool(t, "pool_max_conns=1")
	tbl := scratch(t, p, "id int")
	if err := p.Acquire(ctx, func(c *caduceus.Conn) error {
		_, err := c.Exec(ctx, "BEGIN; INSERT INTO "+tbl+" VALUES (1)")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := rawRows(t, p, "SELECT count(*)::int8 FROM "+tbl, nil); r[0][0][7] != 0 {
		t.Fatal("a transaction left open was committed or is still open")
	}
}

func TestBatchAndCopy(t *testing.T) {
	ctx := t.Context()
	p := livePool(t)
	tbl := scratch(t, p, "id int, n numeric")
	n, err := p.CopyFrom(ctx, tbl, []string{"id", "n"}, &rowsSource{rows: [][]any{{1, runtime.Decimal{Unscaled: 25, Scale: 1}}, {2, nil}}})
	if err != nil || n != 2 {
		t.Fatalf("CopyFrom = %d, %v", n, err)
	}
	var seen []int64
	err = p.Batch(ctx, []runtime.BatchOp{
		{SQL: "INSERT INTO " + tbl + " VALUES ($1, $2)", Args: []any{3, runtime.Decimal{Unscaled: 1}}},
		{SQL: "SELECT count(*)::int8 FROM " + tbl, WantRows: true},
	}, func(i int, rows runtime.Rows, affected int64, err error) error {
		if err != nil {
			return err
		}
		if rows == nil {
			seen = append(seen, affected)
			return nil
		}
		for rows.Next() {
			seen = append(seen, int64(rows.RawValues()[0][7]))
		}
		return nil
	})
	if err != nil || len(seen) != 2 || seen[0] != 1 || seen[1] != 3 {
		t.Fatalf("Batch = %v, %v", seen, err)
	}
	if got := rawRows(t, p, "SELECT n FROM "+tbl+" WHERE id = 1", nil); runtime.Numeric(got[0][0]).String() != "2.5" {
		t.Fatalf("copied numeric = %v", runtime.Numeric(got[0][0]))
	}
}

func TestParseConfigTakesThePoolSettings(t *testing.T) {
	c, err := caduceusdrv.ParseConfig("postgres://u@h/db?pool_max_conns=7&pool_max_conn_idle_time=1m&pool_min_conns=2&application_name=x")
	if err != nil {
		t.Fatal(err)
	}
	kv, err := caduceusdrv.ParseConfig("host=h user=u pool_max_conns=3 application_name='a b'")
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxConns != 7 || c.MaxConnIdleTime != time.Minute {
		t.Errorf("config = %+v", c)
	}
	if kv.MaxConns != 3 || kv.Conn.RuntimeParams["application_name"] != "a b" {
		t.Errorf("key=value config = %+v, %v", kv, kv.Conn.RuntimeParams)
	}
	if _, ok := c.Conn.RuntimeParams["pool_min_conns"]; ok {
		t.Error("pool_min_conns would be sent to the server")
	}
	if c.Conn.RuntimeParams["application_name"] != "x" {
		t.Error("application_name was lost")
	}
	if _, err := caduceusdrv.ParseConfig("postgres://u@h/db?pool_max_conns=none"); err == nil {
		t.Error("a pool_max_conns that is not a number was accepted")
	}
}

type rowsSource struct {
	rows [][]any
	i    int
}

func (s *rowsSource) Next() bool    { s.i++; return s.i <= len(s.rows) }
func (s *rowsSource) Values() []any { return s.rows[s.i-1] }
func (s *rowsSource) Err() error    { return nil }

// reflectPtr returns a pointer to a copy of v, as a generated rowSource
// hands COPY a pointer to each field.
func reflectPtr(v any) any {
	switch x := v.(type) {
	case int:
		return &x
	case runtime.Decimal:
		return &x
	case runtime.Interval:
		return &x
	case runtime.TimeOfDay:
		return &x
	case runtime.TstzRange:
		return &x
	case [][16]byte:
		return &x
	case []int64:
		return &x
	case []string:
		return &x
	case []runtime.Decimal:
		return &x
	case runtime.JSON:
		return &x
	case string:
		return &x
	case bool:
		return &x
	case []byte:
		return &x
	case time.Time:
		return &x
	case [16]byte:
		return &x
	case float64:
		return &x
	case int16:
		return &x
	}
	panic(fmt.Sprintf("no pointer for %T", v))
}
