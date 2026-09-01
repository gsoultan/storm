package runtime

import "errors"

// The constraint violations a service has to tell apart.
//
// Without these every one of them is an opaque driver error, and the handler
// above has to type-assert a pgx type and decode a five-character SQLSTATE to
// decide between 409 and 500 — which is driver knowledge leaking through the
// port that exists to stop exactly that.
//
// Matched with errors.Is:
//
//	if errors.Is(err, runtime.ErrUniqueViolation) { … 409 … }
var (
	// ErrUniqueViolation is a duplicate key (23505).
	ErrUniqueViolation = errors.New("storm: unique constraint violated")
	// ErrForeignKeyViolation is a missing or still-referenced parent (23503).
	ErrForeignKeyViolation = errors.New("storm: foreign key constraint violated")
	// ErrCheckViolation is a CHECK that refused the row (23514).
	ErrCheckViolation = errors.New("storm: check constraint violated")
	// ErrNotNullViolation is a NULL in a NOT NULL column (23502).
	ErrNotNullViolation = errors.New("storm: not-null constraint violated")
	// ErrExclusionViolation is an EXCLUDE constraint refusing an overlap
	// (23P01) — the booking-conflict case, and the reason exclusion
	// constraints are worth having.
	ErrExclusionViolation = errors.New("storm: exclusion constraint violated")

	// ErrSerializationFailure (40001) and ErrDeadlock (40P01) are the two the
	// caller should RETRY rather than report. They are not bugs; they are what
	// concurrency control looks like when it works.
	ErrSerializationFailure = errors.New("storm: serialization failure — retry the transaction")
	ErrDeadlock             = errors.New("storm: deadlock detected — retry the transaction")
)

// ConstraintError names which constraint refused a statement.
//
// It carries SCHEMA metadata — the constraint, table and column names — and
// never a bound value. PostgreSQL's own diagnostic does carry the value
// ("Key (email)=(ada@example.com) already exists"), and that message belongs to
// the server: it is preserved by Unwrap and deliberately not folded into this
// error's own text, so logging a storm error cannot leak a value that logging
// the driver's would.
type ConstraintError struct {
	// Kind is one of the sentinels above.
	Kind error
	// Constraint, Table and Column are as PostgreSQL reported them. Any may be
	// empty: the server does not always populate all three.
	Constraint string
	Table      string
	Column     string
	// Err is the driver's original error, message intact.
	Err error
}

func (e *ConstraintError) Error() string {
	s := e.Kind.Error()
	if e.Constraint != "" {
		s += " (" + e.Constraint + ")"
	} else if e.Table != "" {
		s += " (" + e.Table + ")"
	}
	return s
}

// Unwrap exposes the driver's error, so the server's full diagnostic is one
// errors.As away for whoever wants it.
func (e *ConstraintError) Unwrap() error { return e.Err }

// Is matches the sentinel, so errors.Is(err, ErrUniqueViolation) works on a
// ConstraintError without the caller knowing this type exists.
func (e *ConstraintError) Is(target error) bool { return target == e.Kind }

// Retryable reports whether err is a transient concurrency failure that the
// same transaction, run again, may well succeed at.
//
// A separate question from "which constraint": a 409 for a duplicate is the
// client's problem, and a serialization failure is nobody's — it just needs
// running again.
func Retryable(err error) bool {
	return errors.Is(err, ErrSerializationFailure) || errors.Is(err, ErrDeadlock)
}
