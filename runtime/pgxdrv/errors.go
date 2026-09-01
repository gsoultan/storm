package pgxdrv

import (
	"errors"

	"github.com/gsoultan/storm/runtime"
	"github.com/jackc/pgx/v5/pgconn"
)

// classify turns a driver error into one storm's callers can switch on.
//
// This is the ONLY place a SQLSTATE is read. The Executor port exists so
// driver knowledge cannot reach the rest of the tree, and "type-assert
// *pgconn.PgError and compare 23505" in a handler is exactly that knowledge
// leaking through it.
//
// Anything unrecognised is returned UNCHANGED. A wrapper that renamed every
// error would hide the ones storm has no opinion about, and those are the ones
// worth reading verbatim.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	var kind error
	switch pg.Code {
	case "23505":
		kind = runtime.ErrUniqueViolation
	case "23503":
		kind = runtime.ErrForeignKeyViolation
	case "23514":
		kind = runtime.ErrCheckViolation
	case "23502":
		kind = runtime.ErrNotNullViolation
	case "23P01":
		kind = runtime.ErrExclusionViolation
	case "40001":
		kind = runtime.ErrSerializationFailure
	case "40P01":
		kind = runtime.ErrDeadlock
	default:
		return err
	}
	return &runtime.ConstraintError{
		Kind:       kind,
		Constraint: pg.ConstraintName,
		Table:      pg.TableName,
		Column:     pg.ColumnName,
		Err:        err,
	}
}
