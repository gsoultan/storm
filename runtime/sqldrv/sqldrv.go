// Package sqldrv adapts any database/sql driver to storm's Executor port.
//
// IT IS THE SLOW PATH, ON PURPOSE, and saying so is the point of this comment.
// storm's own clients — runtime/pgxdrv, runtime/mydrv, runtime/msdrv — hand the
// generated scanners raw wire bytes and cost between 0.09 and 1.07 allocations
// per row. A database/sql driver decodes before storm can see the wire, so this
// adapter hands over the DECODED values instead (runtime.Rows.Values), and the
// generated package reads them with runtime/valdec. internal/oraclespike
// measured go-ora at 26.3 allocations per row through that shape.
//
// So this is not an alternative to writing a client. It is what makes a target
// REACHABLE before one exists, and it is how Oracle works today.
//
// WHAT IT BUYS BEYOND ORACLE: any database/sql driver satisfies the port now.
// That was not the reason the second row shape was added, and it is the larger
// consequence of it.
//
// WHAT IT DOES NOT DO, and says rather than degrades:
//
//   - CopyFrom has no database/sql equivalent. Every driver's bulk path is its
//     own API, so this emulates it with one INSERT per row and DOCUMENTS the
//     emulation, which runtime.Executor's contract requires. A performance
//     claim that silently depended on which adapter was passed would be worse
//     than a slow one that says so.
//   - Batch is N round trips. database/sql has no pipelining, so the results
//     are still delivered in order and each is still drained — the semantics
//     hold and the round trips do not collapse.
package sqldrv

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/gsoultan/storm/runtime"
)

// DB is the slice of database/sql this package needs. *sql.DB, *sql.Tx and
// *sql.Conn all satisfy it, so a caller passes whichever the transaction
// boundary calls for — the same choice pgxdrv offers between Conn and Tx.
type DB interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Exec wraps a database/sql handle as a storm Executor.
type Exec struct{ DB DB }

var _ runtime.Executor = Exec{}

// New returns an Executor over any database/sql handle.
func New(db DB) Exec { return Exec{DB: db} }

func (e Exec) Query(ctx context.Context, query string, args []any) (runtime.Rows, error) {
	r, err := e.DB.QueryContext(ctx, query, normalize(args)...)
	if err != nil {
		return nil, err
	}
	cols, err := r.Columns()
	if err != nil {
		r.Close()
		return nil, err
	}
	return newRows(r, len(cols)), nil
}

func (e Exec) Exec(ctx context.Context, query string, args []any) (int64, error) {
	res, err := e.DB.ExecContext(ctx, query, normalize(args)...)
	if err != nil {
		return 0, err
	}
	// RowsAffected is optional in database/sql and several drivers refuse it.
	// A refusal is reported as zero rather than as an error: the statement RAN,
	// and failing the call would turn a driver's limitation into a data error.
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// CopyFrom is EMULATED: one INSERT per row.
//
// runtime.Executor's contract says an adapter whose driver has no copy protocol
// must emulate it and say so in its package documentation — see the note at the
// top of this file. database/sql has no bulk path at all, and every driver's is
// its own API, so there is nothing to reach for generically.
func (e Exec) CopyFrom(ctx context.Context, table string, cols []string,
	src runtime.CopySource) (int64, error) {

	if len(cols) == 0 {
		return 0, fmt.Errorf("sqldrv: CopyFrom into %s names no columns", table)
	}
	stmt := insertStmt(table, cols)
	var n int64
	for src.Next() {
		if _, err := e.DB.ExecContext(ctx, stmt, normalize(src.Values())...); err != nil {
			return n, err
		}
		n++
	}
	return n, src.Err()
}

// Batch is N round trips, delivered in order.
//
// database/sql has no pipelining, so the round trips do not collapse — but the
// SEMANTICS hold: each result is handed to `each` in order, exactly one of rows
// and affected is meaningful, and the first error from `each` aborts the rest.
func (e Exec) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {

	for i, op := range ops {
		if op.WantRows {
			r, err := e.Query(ctx, op.SQL, op.Args)
			if each != nil {
				if cbErr := each(i, r, 0, err); cbErr != nil {
					if r != nil {
						r.Close()
					}
					return cbErr
				}
			}
			if r != nil {
				// Drained even when there is no callback: anything else leaves
				// a result set open on the connection.
				for r.Next() {
				}
				r.Close()
			}
			if err != nil {
				return err
			}
			continue
		}
		n, err := e.Exec(ctx, op.SQL, op.Args)
		if each != nil {
			if cbErr := each(i, nil, n, err); cbErr != nil {
				return cbErr
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// rows is the VALUE shape of runtime.Rows: RawValues is nil and Values carries
// what the driver decoded. See runtime.Rows for the contract.
type rows struct {
	r    *sql.Rows
	vals []any
	ptrs []any
	err  error
}

func newRows(r *sql.Rows, n int) *rows {
	x := &rows{r: r, vals: make([]any, n), ptrs: make([]any, n)}
	for i := range x.vals {
		x.ptrs[i] = &x.vals[i]
	}
	return x
}

func (x *rows) Next() bool {
	if x.err != nil || !x.r.Next() {
		return false
	}
	// Into the SAME slice every row, which is why runtime.Rows documents a
	// value as valid only until the next Next — and why valdec.Bytes copies.
	if err := x.r.Scan(x.ptrs...); err != nil {
		x.err = err
		return false
	}
	return true
}

// RawValues is nil: this adapter is the VALUE shape, because the driver decoded
// before storm could see the wire. Re-encoding what it just decoded so this
// could return something would be slower than reading the values.
func (*rows) RawValues() [][]byte { return nil }

func (x *rows) Values() []any { return x.vals }

func (x *rows) Close() { _ = x.r.Close() }

func (x *rows) Err() error {
	if x.err != nil {
		return x.err
	}
	return x.r.Err()
}

// insertStmt is the per-row statement CopyFrom emulates with.
//
// PLACEHOLDERS ARE THE CALLER'S PROBLEM, and this is where that shows: `?`,
// `$n`, `:n` and `@pn` are four spellings and database/sql has no opinion
// about which a driver takes. The default is `:n`, because the target that
// forced this adapter into existence uses it; Placeholder changes it.
func insertStmt(table string, cols []string) string {
	q := make([]byte, 0, 64)
	q = append(q, "INSERT INTO "...)
	q = append(q, table...)
	q = append(q, " ("...)
	for i, c := range cols {
		if i > 0 {
			q = append(q, ", "...)
		}
		q = append(q, c...)
	}
	q = append(q, ") VALUES ("...)
	for i := range cols {
		if i > 0 {
			q = append(q, ", "...)
		}
		q = append(q, Placeholder(i+1)...)
	}
	q = append(q, ')')
	return string(q)
}

// Placeholder renders the n-th bind marker. Replaceable because database/sql
// has no opinion and the four targets storm knows spell it four ways.
var Placeholder = func(n int) string { return ":" + itoa(n) }

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
