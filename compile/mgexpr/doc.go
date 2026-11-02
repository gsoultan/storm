// Package mgexpr translates the part of SQL's expression language a MongoDB
// server can evaluate with SQL's answer: a CHECK constraint into a
// validator's $expr, and a partial index's WHERE into a
// partialFilterExpression.
//
// The subset is small on purpose, and everything outside it is an error that
// names the construct, never an approximation:
//
//   - columns of the table, and integer, decimal, string, boolean and NULL
//     literals (a literal may carry a cast to its own type);
//   - = <> != < <= > >=, AND, OR, NOT, IS [NOT] NULL, [NOT] IN (...),
//     [NOT] BETWEEN, [NOT] LIKE, + - * and unary minus;
//   - length/char_length, abs and coalesce.
//
// Three things decide what is in it. NULL: SQL's logic has three values and a
// CHECK fails only on FALSE, so every translated operator carries UNKNOWN
// through explicitly — MongoDB's own comparison orders null below every number
// and would call `NULL >= 0` false. Types: a comparison is translated only
// between values of one kind, since MongoDB compares across BSON types by a
// fixed type order where SQL would cast or refuse. Ordering: text is compared
// for equality only — PostgreSQL orders it by a collation and MongoDB by
// bytes — and an enum the same, since PostgreSQL orders it by declaration.
// Division is not translated: SQL's integer division truncates, $divide does
// not.
package mgexpr

import "errors"

// ErrUnsupported wraps every refusal.
var ErrUnsupported = errors.New("no MongoDB form")

func fail(what string) error { return errors.Join(ErrUnsupported, errors.New(what)) }
