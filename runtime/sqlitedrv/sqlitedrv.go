// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package sqlitedrv is the SQLite port: runtime.Executor over database/sql.
//
// THE DRIVER IS YOURS, as it is for Oracle (runtime/sqldrv). storm's module
// stays standard library plus caduceus, so it imports no SQLite driver; a
// program blank-imports one and hands storm the *sql.DB. storm is tested
// against modernc.org/sqlite, a pure-Go translation of SQLite that needs no
// cgo — `storm bootstrap -dialect sqlite` writes that import — and nothing
// here is specific to it except DSN, which spells modernc's connection
// options.
//
// What this adds to runtime/sqldrv is the VALUES. SQLite stores what it is
// given, under type affinity rather than a declared type, so the shape a Go
// value is bound in IS the shape it is stored in, compared in and sorted in:
//
//   - A time.Time is bound as fixed-width UTC text,
//     2006-01-02T15:04:05.000000000Z — the form compile/liteddl stores and
//     compile/sqlite's clock writes. A driver left to itself writes
//     time.Time.String(), which carries a monotonic-clock suffix it cannot
//     parse back, and a local offset that breaks text order.
//   - A runtime.TimeOfDay is bound as 15:04:05.000000, fixed width for the
//     same reason.
//   - A runtime.JSON is bound as TEXT. As a BLOB, SQLite 3.45 and later read
//     it as their binary JSONB encoding, and json_valid() refuses it.
//   - A slice — the keys of an In, the roots of a traversal — is bound as ONE
//     JSON array, which the lowering unpacks with json_each. A uuid travels
//     inside it as hex.
//
// CopyFrom is multi-row INSERTs, as many rows per statement as SQLite's
// parameter limit allows, and inside a transaction when the caller is not
// already in one: a bulk load that fails half-way leaves nothing, as COPY
// does.
package sqlitedrv

import (
	"context"
	"errors"
	"time"

	"github.com/gsoultan/storm/v2/runtime"
	"github.com/gsoultan/storm/v2/runtime/sqldrv"
)

// DB is what an Exec runs against: a *sql.DB, a *sql.Tx or a *sql.Conn.
type DB = sqldrv.DB

// Exec is runtime.Executor over one database/sql handle.
type Exec struct{ DB DB }

var _ runtime.Executor = Exec{}

// New wraps a handle.
func New(db DB) Exec { return Exec{DB: db} }

func (e Exec) inner() sqldrv.Exec { return sqldrv.Exec{DB: e.DB} }

// Query runs a read.
func (e Exec) Query(ctx context.Context, query string, args []any) (runtime.Rows, error) {
	return e.inner().Query(ctx, query, bind(args))
}

// Exec runs a write and reports the rows it touched.
func (e Exec) Exec(ctx context.Context, query string, args []any) (int64, error) {
	return e.inner().Exec(ctx, query, bind(args))
}

// Batch runs each operation in turn. SQLite is in-process, so a round trip
// is a function call and there is nothing to pipeline.
func (e Exec) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {
	bound := make([]runtime.BatchOp, len(ops))
	for i, op := range ops {
		op.Args = bind(op.Args)
		bound[i] = op
	}
	return e.inner().Batch(ctx, bound, each)
}

// CopyFrom loads rows with multi-row INSERTs. It is atomic only when e runs
// inside a transaction; Pool.CopyFrom opens one.
func (e Exec) CopyFrom(ctx context.Context, table string, cols []string,
	src runtime.CopySource) (int64, error) {
	return copyRows(ctx, e.DB, table, cols, src)
}

// maxParams is SQLite's SQLITE_MAX_VARIABLE_NUMBER since 3.32. A statement
// with more parameters than this fails to prepare.
const maxParams = 32766

// maxRowsPerInsert caps one statement's rows below what the parameter limit
// alone would allow: past a few hundred rows a longer statement buys nothing
// and costs a larger prepare.
const maxRowsPerInsert = 500

func copyRows(ctx context.Context, db DB, table string, cols []string, src runtime.CopySource) (int64, error) {
	if len(cols) == 0 {
		return 0, errors.New("sqlitedrv: CopyFrom into " + table + " names no columns")
	}
	per := maxParams / len(cols)
	if per > maxRowsPerInsert {
		per = maxRowsPerInsert
	}
	if per < 1 {
		return 0, errors.New("sqlitedrv: " + table + " has more columns than SQLite binds in one statement")
	}
	var (
		n     int64
		args  = make([]any, 0, per*len(cols))
		rows  int
		stmts = map[int]string{}
	)
	flush := func() error {
		if rows == 0 {
			return nil
		}
		q, ok := stmts[rows]
		if !ok {
			q = insertRows(table, cols, rows)
			stmts[rows] = q
		}
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			return err
		}
		n += int64(rows)
		rows, args = 0, args[:0]
		return nil
	}
	for src.Next() {
		vals := src.Values()
		if len(vals) != len(cols) {
			return n, errors.New("sqlitedrv: CopyFrom into " + table + ": a row's width differs from its column list")
		}
		for _, v := range vals {
			args = append(args, value(v))
		}
		rows++
		if rows == per {
			if err := flush(); err != nil {
				return n, err
			}
		}
	}
	if err := src.Err(); err != nil {
		return n, err
	}
	return n, flush()
}

// insertRows is INSERT INTO table (cols) VALUES (?1, …), (…), … for rows rows.
func insertRows(table string, cols []string, rows int) string {
	q := make([]byte, 0, 32+len(cols)*(rows*6+12))
	q = append(q, "INSERT INTO "...)
	q = appendIdent(q, table)
	q = append(q, " ("...)
	for i, c := range cols {
		if i > 0 {
			q = append(q, ", "...)
		}
		q = appendIdent(q, c)
	}
	q = append(q, ") VALUES "...)
	ord := 0
	for r := 0; r < rows; r++ {
		if r > 0 {
			q = append(q, ", "...)
		}
		q = append(q, '(')
		for i := range cols {
			if i > 0 {
				q = append(q, ", "...)
			}
			ord++
			q = append(q, '?')
			q = appendInt(q, int64(ord))
		}
		q = append(q, ')')
	}
	return string(q)
}

// appendIdent quotes a name, as compile/sqlite.Ident does. The names come from
// generated code, never from a value, but a quote in one would still break the
// statement.
func appendIdent(q []byte, s string) []byte {
	q = append(q, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			q = append(q, '"')
		}
		q = append(q, s[i])
	}
	return append(q, '"')
}

// bind converts every argument to the shape it is stored in; see the package
// comment. It copies only when something changes.
func bind(args []any) []any {
	out := args
	copied := false
	for i, a := range args {
		v := value(a)
		if same(v, a) {
			continue
		}
		if !copied {
			out = append([]any(nil), args...)
			copied = true
		}
		out[i] = v
	}
	return out
}

// same reports whether value left an argument as it was. Only the kinds value
// passes through unchanged can compare equal, and those are all comparable.
func same(v, a any) bool {
	switch a.(type) {
	case nil, bool, int64, float64, string:
		return v == a
	}
	return false
}

// TimestampFormat is the one shape every temporal value is stored in. It must
// match compile/liteddl.TimestampFormat and compile/sqlite.TimestampFormat; a
// test holds the three together.
const TimestampFormat = "2006-01-02T15:04:05.000000000Z"

// value is one argument in the shape it is stored in.
func value(a any) any {
	switch x := a.(type) {
	case nil, bool, int64, float64, string:
		return a
	case time.Time:
		return formatTime(x)
	case *time.Time:
		if x == nil {
			return nil
		}
		return formatTime(*x)
	case runtime.TimeOfDay:
		return formatTimeOfDay(x)
	case *runtime.TimeOfDay:
		if x == nil {
			return nil
		}
		return formatTimeOfDay(*x)
	case runtime.JSON:
		if x == nil {
			return nil
		}
		return string(x)
	case *runtime.JSON:
		if x == nil || *x == nil {
			return nil
		}
		return string(*x)
	case *[16]byte:
		if x == nil {
			return nil
		}
		return append([]byte(nil), x[:]...)
	case *runtime.Decimal:
		if x == nil {
			return nil
		}
		return x.String()
	}
	if doc, ok := jsonList(a); ok {
		return doc
	}
	return a
}

func formatTime(t time.Time) string {
	var b [len(TimestampFormat)]byte
	return string(t.UTC().AppendFormat(b[:0], TimestampFormat))
}

// formatTimeOfDay writes HH:MM:SS.ffffff, fixed width so text order is time
// order. Hand-written: fmt is not used in runtime/.
func formatTimeOfDay(t runtime.TimeOfDay) string {
	h, m, s, us := t.Parts()
	b := make([]byte, 0, 15)
	b = append2(b, h)
	b = append(b, ':')
	b = append2(b, m)
	b = append(b, ':')
	b = append2(b, s)
	b = append(b, '.')
	for d := 100000; d > 0; d /= 10 {
		b = append(b, byte('0'+us/d%10))
	}
	return string(b)
}

func append2(b []byte, n int) []byte { return append(b, byte('0'+n/10%10), byte('0'+n%10)) }

func appendInt(b []byte, n int64) []byte {
	if n == 0 {
		return append(b, '0')
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return append(b, buf[i:]...)
}
