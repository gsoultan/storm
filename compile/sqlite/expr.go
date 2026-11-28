// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package sqlite

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/storm/v2/schema"
)

// Expression rendering for SQLite.
//
// compile/pgsql's renderer with the PostgreSQL facts taken out, and errors
// accumulated on the writer as compile/mysql's are — these render deep inside
// a tree walk, and a returned error at every node would be noise.
//
// What differs from PostgreSQL, and why each one is a RESULT difference rather
// than a spelling one:
//
//   - Division. SQLite's `/` on two integers truncates, as PostgreSQL's does,
//     and a NUMERIC here is stored as an integer whenever it is whole — so
//     `price / 2` truncates on a row priced 3 and not on one priced 3.5. The
//     dividend is CAST to REAL unconditionally, and the result rounded to the
//     declared scale.
//   - Every numeric aggregate is rounded to its declared scale. A sum over
//     NUMERIC(10,2) is float arithmetic here, and 0.1 + 0.2 is
//     0.30000000000000004; the declaration said two places.
//   - Literals: a decimal is its digits, a boolean is 1 or 0, and a timestamp
//     is the fixed-width UTC text compile/liteddl stores — compared as TEXT,
//     so any other shape compares wrong rather than failing.
//   - date_trunc does not exist. It is rebuilt from strftime for the units
//     whose boundary is a calendar field, and refused for the rest.
//   - GROUPING SETS, ROLLUP, CUBE and GROUPING() do not exist, and neither
//     does array_agg, which is what PostgreSQL's first-value spelling
//     collects into. Refused rather than emulated: a ROLLUP rewritten as a
//     UNION of groupings is a different statement with a different cost.

// ErrNoGroupingSets is why a ROLLUP, CUBE or GROUPING SETS cannot be lowered.
var ErrNoGroupingSets = errors.New(
	"compile/sqlite: SQLite has no GROUPING SETS, ROLLUP or CUBE; declare one aggregation " +
		"per grouping instead")

// ErrNoFirstOf is why a first-value aggregate cannot be lowered: PostgreSQL's
// spelling collects the group into an array, and SQLite has no array.
var ErrNoFirstOf = errors.New(
	"compile/sqlite: a first value in a declared order has no SQLite form storm writes yet")

// ErrLiteralNonFinite is why a NaN or infinite float constant is refused:
// SQLite stores NaN as NULL, so the constant would silently become one.
var ErrLiteralNonFinite = errors.New("compile/sqlite: SQLite has no NaN or infinite REAL literal")

// exprWriter renders expressions, carrying the first thing it could not lower.
type exprWriter struct {
	b   strings.Builder
	err error
}

func (w *exprWriter) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}

// Expr renders one expression.
func Expr(e schema.Expr) (string, error) {
	var w exprWriter
	w.writeExpr(e)
	return w.b.String(), w.err
}

// Cond renders one declared predicate.
func Cond(c schema.Cond) (string, error) {
	var w exprWriter
	w.writeCond(c)
	return w.b.String(), w.err
}

func (w *exprWriter) writeExpr(e schema.Expr) {
	switch e.Kind {
	case schema.ExprCol:
		if e.Tbl != "" {
			w.b.WriteString(Ident(e.Tbl))
			w.b.WriteByte('.')
		}
		w.b.WriteString(Ident(e.Col))

	case schema.ExprStar:
		w.b.WriteString("*")

	case schema.ExprLit:
		w.literal(e.Lit)

	case schema.ExprParam:
		// Numbered at generate time, as PostgreSQL's are: `?2` is SQLite's
		// numbered parameter, and the same number used in two union branches
		// binds ONE value — the property a union's declared parameter needs.
		w.b.WriteString(Placeholder)
		w.b.WriteString(strconv.Itoa(e.Param + 1))

	case schema.ExprFunc:
		if e.Fn == "date_trunc" {
			w.dateTrunc(e)
			return
		}
		w.b.WriteString(e.Fn)
		w.b.WriteByte('(')
		w.writeArgs(e.Args)
		w.b.WriteByte(')')

	case schema.ExprBinary:
		w.writeBinary(e)

	case schema.ExprField:
		w.b.WriteString(JSONField(Ident(e.Tbl)+".\"value\"", e.Col))

	case schema.ExprGrouping:
		w.fail(ErrNoGroupingSets)

	case schema.ExprAgg:
		if e.Fn == string(schema.AggFirst) {
			w.fail(ErrNoFirstOf)
			return
		}
		round := e.Type.Name == schema.TypeNumeric && !e.Type.Array &&
			e.Fn != string(schema.AggCount)
		if round {
			w.b.WriteString("round(")
		}
		w.b.WriteString(e.Fn)
		w.b.WriteByte('(')
		if e.Distinct {
			w.b.WriteString("DISTINCT ")
		}
		w.writeArgs(e.Args)
		w.b.WriteByte(')')
		// FILTER binds to the aggregate and precedes OVER, as in PostgreSQL;
		// SQLite has had it since 3.30.
		if e.Filter != nil {
			w.b.WriteString(" FILTER (WHERE ")
			w.writeCond(*e.Filter)
			w.b.WriteByte(')')
		}
		if e.Over != nil {
			w.writeOver(*e.Over)
		}
		if round {
			w.b.WriteString(", ")
			w.b.WriteString(strconv.Itoa(e.Type.Scale))
			w.b.WriteByte(')')
		}

	case schema.ExprWindow:
		w.b.WriteString(e.Fn)
		w.b.WriteByte('(')
		w.writeArgs(e.Args)
		w.b.WriteByte(')')
		if e.Over != nil {
			w.writeOver(*e.Over)
		}
	}
}

// JSONField reads one key of a JSON object as TEXT, which is what
// PostgreSQL's ->> answers: NULL when the key is absent, the string's
// characters for a string, and the JSON spelling for anything else.
//
// SQLite's own ->> is close and not the same. It answers a number as an
// INTEGER or a REAL and a boolean as 1 or 0, so `qty = '3'` is false here —
// an integer never equals a text — and `flag = 'true'` is false everywhere.
// The CASE restores the text PostgreSQL would have produced.
func JSONField(doc, key string) string {
	path := "'$.\"" + strings.ReplaceAll(strings.ReplaceAll(key, `"`, `\"`), "'", "''") + "\"'"
	return "CASE json_type(" + doc + ", " + path + ") WHEN 'true' THEN 'true' " +
		"WHEN 'false' THEN 'false' ELSE CAST(json_extract(" + doc + ", " + path + ") AS TEXT) END"
}

// dateTrunc rebuilds PostgreSQL's date_trunc from strftime, producing the same
// fixed-width UTC text every timestamp is stored as, so the result groups,
// sorts and decodes like a stored one.
//
// Only the units whose boundary is a calendar FIELD are rebuilt. A week starts
// on Monday, as it does in PostgreSQL (ISO 8601). The rest — decade, century,
// millennium, microseconds — are refused: they are rare, and getting one
// subtly wrong is worse than saying so.
func (w *exprWriter) dateTrunc(e schema.Expr) {
	if len(e.Args) != 2 || e.Args[0].Kind != schema.ExprLit {
		w.fail(errors.New("compile/sqlite: date_trunc wants a literal unit"))
		return
	}
	var ts exprWriter
	ts.writeExpr(e.Args[1])
	if ts.err != nil {
		w.fail(ts.err)
		return
	}
	t := ts.b.String()
	const tail = ".000000000Z"
	switch unit := e.Args[0].Lit.S; unit {
	case "milliseconds":
		w.b.WriteString("(strftime('%Y-%m-%dT%H:%M:%f', " + t + ") || '000000Z')")
	case "second":
		w.b.WriteString("strftime('%Y-%m-%dT%H:%M:%S" + tail + "', " + t + ")")
	case "minute":
		w.b.WriteString("strftime('%Y-%m-%dT%H:%M:00" + tail + "', " + t + ")")
	case "hour":
		w.b.WriteString("strftime('%Y-%m-%dT%H:00:00" + tail + "', " + t + ")")
	case "day":
		w.b.WriteString("strftime('%Y-%m-%dT00:00:00" + tail + "', " + t + ")")
	case "week":
		// Forward to the coming Sunday (or stay, on one), then back six days:
		// the Monday that starts the ISO week.
		w.b.WriteString("strftime('%Y-%m-%dT00:00:00" + tail + "', " + t +
			", 'weekday 0', '-6 days')")
	case "month":
		w.b.WriteString("strftime('%Y-%m-01T00:00:00" + tail + "', " + t + ")")
	case "quarter":
		w.b.WriteString("(strftime('%Y-', " + t + ") || printf('%02d', " +
			"((CAST(strftime('%m', " + t + ") AS INTEGER) - 1) / 3) * 3 + 1) || " +
			"'-01T00:00:00" + tail + "')")
	case "year":
		w.b.WriteString("strftime('%Y-01-01T00:00:00" + tail + "', " + t + ")")
	default:
		w.fail(fmt.Errorf("compile/sqlite: date_trunc(%q, …) has no SQLite form storm writes; "+
			"second, minute, hour, day, week, month, quarter and year do", unit))
	}
}

// writeBinary renders arithmetic, always parenthesised: the IR is a tree, so
// the grouping is already decided and precedence must not re-decide it.
func (w *exprWriter) writeBinary(e schema.Expr) {
	if len(e.Args) != 2 {
		return
	}
	if e.Arith == schema.ArithDiv {
		// See the file comment: CAST, because a whole NUMERIC is an integer
		// here and integer division truncates; round, because the declaration
		// gave a scale. Division by zero is NULL in SQLite rather than an
		// error, which is what a nullif() guard would have produced anyway.
		w.b.WriteString("round(CAST(")
		w.writeExpr(e.Args[0])
		w.b.WriteString(" AS REAL) / ")
		w.writeExpr(e.Args[1])
		w.b.WriteString(", ")
		w.b.WriteString(strconv.Itoa(e.Type.Scale))
		w.b.WriteByte(')')
		return
	}
	w.b.WriteByte('(')
	w.writeExpr(e.Args[0])
	switch e.Arith {
	case schema.ArithAdd:
		w.b.WriteString(" + ")
	case schema.ArithSub:
		w.b.WriteString(" - ")
	case schema.ArithMul:
		w.b.WriteString(" * ")
	default:
		// Not " ? ", which pgsql falls through to: `?` is a PARAMETER here,
		// and an unknown operator would silently add one to the statement.
		w.fail(fmt.Errorf("compile/sqlite: no lowering for arithmetic operator %v", e.Arith))
	}
	w.writeExpr(e.Args[1])
	w.b.WriteByte(')')
}

func (w *exprWriter) writeArgs(args []schema.Expr) {
	for i, a := range args {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.writeExpr(a)
	}
}

// writeOrderBy renders ORDER BY and its terms, for a window. NULLS FIRST and
// NULLS LAST exist here (3.30+), so the placement is kept as declared.
func (w *exprWriter) writeOrderBy(terms []schema.OrderTerm) {
	w.b.WriteString("ORDER BY ")
	for i, t := range terms {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.writeExpr(t.Expr)
		if t.Desc {
			w.b.WriteString(" DESC")
		}
		if t.NullsFirst != nil {
			if *t.NullsFirst {
				w.b.WriteString(" NULLS FIRST")
			} else {
				w.b.WriteString(" NULLS LAST")
			}
		}
	}
}

func (w *exprWriter) writeOver(win schema.Window) {
	w.b.WriteString(" OVER (")
	if len(win.PartitionBy) > 0 {
		w.b.WriteString("PARTITION BY ")
		w.writeArgs(win.PartitionBy)
	}
	if len(win.OrderBy) > 0 {
		if len(win.PartitionBy) > 0 {
			w.b.WriteByte(' ')
		}
		w.writeOrderBy(win.OrderBy)
	}
	if win.Frame != nil {
		if win.Frame.Kind == schema.FrameRange {
			w.b.WriteString(" RANGE BETWEEN ")
		} else {
			w.b.WriteString(" ROWS BETWEEN ")
		}
		w.writeBound(win.Frame.Start)
		w.b.WriteString(" AND ")
		w.writeBound(win.Frame.End)
	}
	w.b.WriteByte(')')
}

func (w *exprWriter) writeBound(bound schema.FrameBound) {
	switch bound.Kind {
	case schema.UnboundedPreceding:
		w.b.WriteString("UNBOUNDED PRECEDING")
	case schema.Preceding:
		w.b.WriteString(strconv.Itoa(bound.N))
		w.b.WriteString(" PRECEDING")
	case schema.CurrentRow:
		w.b.WriteString("CURRENT ROW")
	case schema.Following:
		w.b.WriteString(strconv.Itoa(bound.N))
		w.b.WriteString(" FOLLOWING")
	case schema.UnboundedFollowing:
		w.b.WriteString("UNBOUNDED FOLLOWING")
	}
}

func (w *exprWriter) writeCond(c schema.Cond) {
	switch c.Kind {
	case schema.CondCmp:
		w.writeExpr(c.Left)
		w.b.WriteByte(' ')
		w.b.WriteString(string(c.Op))
		w.b.WriteByte(' ')
		w.writeExpr(c.Right)

	case schema.CondIsNull:
		w.writeExpr(c.Left)
		w.b.WriteString(" IS NULL")

	case schema.CondIsNotNull:
		w.writeExpr(c.Left)
		w.b.WriteString(" IS NOT NULL")

	case schema.CondNot:
		w.b.WriteString("NOT (")
		if len(c.Args) > 0 {
			w.writeCond(c.Args[0])
		}
		w.b.WriteByte(')')

	case schema.CondAnd, schema.CondOr:
		sep := " AND "
		if c.Kind == schema.CondOr {
			sep = " OR "
		}
		w.b.WriteByte('(')
		for i, a := range c.Args {
			if i > 0 {
				w.b.WriteString(sep)
			}
			w.writeCond(a)
		}
		w.b.WriteByte(')')
	}
}

// literal renders a declaration-time constant in SQLite's spelling. Not
// schema.Literal.SQL, which is PostgreSQL's: `'1.50'::numeric` and
// `::timestamptz` are syntax errors here.
func (w *exprWriter) literal(l schema.Literal) {
	switch l.Kind {
	case schema.TypeText, schema.TypeVarchar:
		w.b.WriteString(quote(l.S))
	case schema.TypeInt2, schema.TypeInt4, schema.TypeInt8:
		w.b.WriteString(strconv.FormatInt(l.I, 10))
	case schema.TypeFloat4, schema.TypeFloat8:
		if math.IsNaN(l.F) || math.IsInf(l.F, 0) {
			w.fail(ErrLiteralNonFinite)
			return
		}
		// An exponent keeps it a REAL: a bare 2 would be an INTEGER, and
		// `x / 2` would divide as integers.
		w.b.WriteString(strconv.FormatFloat(l.F, 'e', -1, 64))
	case schema.TypeNumeric:
		if !isDecimalText(l.S) {
			w.fail(fmt.Errorf("compile/sqlite: decimal literal %q", l.S))
			return
		}
		w.b.WriteString(l.S)
	case schema.TypeTimestamptz:
		ts, err := time.Parse(time.RFC3339Nano, l.S)
		if err != nil {
			w.fail(fmt.Errorf("compile/sqlite: timestamp literal %q: %w", l.S, err))
			return
		}
		w.b.WriteString(quote(ts.UTC().Format(TimestampFormat)))
	case schema.TypeBool:
		if l.B {
			w.b.WriteString("1")
		} else {
			w.b.WriteString("0")
		}
	default:
		w.fail(fmt.Errorf("compile/sqlite: a %s literal has no SQLite spelling storm writes", l.Kind))
	}
}

// TimestampFormat is how every temporal value is stored and bound; it must
// match compile/liteddl.TimestampFormat and runtime/sqlitedrv, and a test
// holds the three together.
const TimestampFormat = "2006-01-02T15:04:05.000000000Z"

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// isDecimalText reports whether s is an optionally signed decimal numeral:
// what Decimal.String produces, and nothing a statement could be bent with.
func isDecimalText(s string) bool {
	if s != "" && s[0] == '-' {
		s = s[1:]
	}
	digits, dot := 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return digits > 0
}

// GroupBy renders the grouping clause. GROUPING SETS forms are refused.
func GroupBy(agg *schema.Aggregate) (string, error) {
	if len(agg.By) == 0 {
		return "", nil
	}
	if agg.Sets != nil {
		return "", ErrNoGroupingSets
	}
	var w exprWriter
	w.b.WriteString(" GROUP BY ")
	for i, g := range agg.By {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.writeExpr(g.Expr)
	}
	return w.b.String(), w.err
}
