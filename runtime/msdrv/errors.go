package msdrv

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gsoultan/storm/runtime"
)

// Errors, and the one rule that governs them: an error NEVER carries a value.
//
// SQL Server's messages name the constraint and the table, and for a unique
// violation they also name the DUPLICATE KEY — "The duplicate key value is
// (alice@example.com)". That is a row's data in a string that ends up in logs,
// in an error-tracking service, and in a support ticket. runtime/mydrv's
// errors.go makes the same cut for the same reason; here there is more to cut,
// because SQL Server is more helpful.
//
// So Error keeps the number, the class, the procedure and the constraint NAME,
// and the message text is kept only far enough to find the name in it.

// Error is a message the server sent.
type Error struct {
	// Number is the server's error number. 2627 is a unique violation, 547 a
	// foreign key or check violation, 515 a NOT NULL violation.
	Number int32
	// Class is severity. 11 and above is an error a caller sees; 16 is the
	// usual "you did something wrong"; 20 and above kills the connection.
	Class uint8
	// State disambiguates a number that several code paths raise.
	State uint8
	// Proc is the stored procedure that raised it, or "".
	Proc string
	// Line is the line within the batch or procedure.
	Line int32
	// Constraint is the index or constraint the server named, or "".
	Constraint string
	// Table is the object the server named, or "".
	Table string

	// msg is the server's own text. NOT printed by Error(), and reachable only
	// through ServerMessage — see its note.
	msg string

	// kindHint is the one word of the message text that says which of the two
	// conditions error 547 is. Unexported and never printed: it is a keyword
	// the server emits, not the caller's data, and it exists only because the
	// server refuses to distinguish foreign key from check any other way.
	kindHint string
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "msdrv: server error %d (class %d", e.Number, e.Class)
	if e.State != 0 {
		fmt.Fprintf(&b, ", state %d", e.State)
	}
	b.WriteByte(')')
	if e.Constraint != "" {
		b.WriteString(" on ")
		b.WriteString(e.Constraint)
	}
	if e.Table != "" {
		b.WriteString(" in ")
		b.WriteString(e.Table)
	}
	if e.Proc != "" {
		fmt.Fprintf(&b, " at %s:%d", e.Proc, e.Line)
	}
	// No message text. See the note at the top of this file: SQL Server puts
	// the offending VALUE in it, and a value in an error is a value in a log.
	return b.String()
}

// ServerMessage is the server's own message text.
//
// It MAY CONTAIN VALUES — "The duplicate key value is (alice@example.com)" —
// which is why Error() does not include it and why this is a method a caller
// has to reach for deliberately. Useful at a debugger or in a test; a log line
// built from it is a log line with a row's data in it.
func (e *Error) ServerMessage() string { return e.msg }

// Server error numbers storm classifies.
const (
	errUniqueViolation     = 2627 // PRIMARY KEY or UNIQUE constraint
	errUniqueIndex         = 2601 // UNIQUE INDEX, which is a different number
	errConstraintViolation = 547  // FOREIGN KEY or CHECK
	errNotNullViolation    = 515
	errStringTruncated     = 8152
	errStringTruncated2    = 2628 // the 2016+ message, which names the column
	errArithmeticOverflow  = 8115
	errDeadlock            = 1205
	errLockTimeout         = 1222
	errDivideByZero        = 8134
	errCancelled           = 3621 // "the statement has been terminated"
)

// readMessage reads an ERROR or INFO token.
func (x *conn) readMessage() (*Error, error) {
	n, err := x.readU16()
	if err != nil {
		return nil, err
	}
	start := x.consumed()

	num, err := x.readU32()
	if err != nil {
		return nil, err
	}
	state, err := x.readByte()
	if err != nil {
		return nil, err
	}
	class, err := x.readByte()
	if err != nil {
		return nil, err
	}
	msg, err := x.readUSVarchar()
	if err != nil {
		return nil, err
	}
	if _, err := x.readBVarchar(); err != nil { // server name
		return nil, err
	}
	proc, err := x.readBVarchar()
	if err != nil {
		return nil, err
	}
	line, err := x.readU32()
	if err != nil {
		return nil, err
	}
	// Bounded by the declared length rather than by trusting the parse: a
	// server that adds a field to this token in some future version would
	// otherwise desynchronise every connection.
	if rem := int(n) - (x.consumed() - start); rem > 0 {
		if err := x.skip(rem); err != nil {
			return nil, err
		}
	}

	e := &Error{
		Number: int32(num), Class: class, State: state,
		Proc: proc, Line: int32(line), msg: msg,
	}
	e.Constraint, e.Table = namesIn(int32(num), msg)
	if int32(num) == errConstraintViolation {
		switch {
		case strings.Contains(msg, "CHECK constraint"):
			e.kindHint = "CHECK"
		case strings.Contains(msg, "FOREIGN KEY constraint"):
			e.kindHint = "FOREIGN KEY"
		}
	}
	return e, nil
}

// namesIn pulls the constraint and object names out of a message, and nothing
// else.
//
// The messages this reads are stable across versions because they are part of
// the product's documented surface:
//
//	2627: Violation of UNIQUE KEY constraint 'uq_users_email'. Cannot insert
//	      duplicate key in object 'dbo.users'. The duplicate key value is (…).
//	2601: Cannot insert duplicate key row in object 'dbo.users' with unique
//	      index 'uq_users_email'. The duplicate key value is (…).
//	 547: The INSERT statement conflicted with the FOREIGN KEY constraint
//	      "fk_members_org". The conflict occurred in database "storm", table
//	      "dbo.ms_orgs", column 'id'.
//	 515: Cannot insert the value NULL into column 'email', table 'db.dbo.t'.
//
// The tail — "The duplicate key value is (alice@example.com)" — is exactly what
// must not survive, which is why this returns two names rather than a trimmed
// message.
func namesIn(number int32, msg string) (constraint, table string) {
	switch number {
	case errUniqueViolation:
		return quoted(msg, '\''), secondQuoted(msg, '\'')
	case errUniqueIndex:
		// The object comes FIRST here and the index second, which is the
		// opposite order to 2627. Two numbers, two message shapes; reading both
		// the same way puts the table in the constraint field.
		return secondQuoted(msg, '\''), quoted(msg, '\'')
	case errConstraintViolation:
		return quoted(msg, '"'), secondQuoted(msg, '"')
	case errNotNullViolation:
		return quoted(msg, '\''), secondQuoted(msg, '\'')
	}
	return "", ""
}

// quoted returns the first q-delimited run, or "".
func quoted(s string, q byte) string {
	i := strings.IndexByte(s, q)
	if i < 0 {
		return ""
	}
	j := strings.IndexByte(s[i+1:], q)
	if j < 0 {
		return ""
	}
	return s[i+1 : i+1+j]
}

// secondQuoted returns the second q-delimited run, or "".
func secondQuoted(s string, q byte) string {
	i := strings.IndexByte(s, q)
	if i < 0 {
		return ""
	}
	j := strings.IndexByte(s[i+1:], q)
	if j < 0 {
		return ""
	}
	return quoted(s[i+1+j+1:], q)
}

// classify maps a server error to storm's ConstraintError, so a caller branches
// on what happened rather than on a number.
//
// Anything unrecognised is returned UNCHANGED, for the reason runtime/mydrv
// gives: a wrapper that renamed every error would hide the ones storm has no
// opinion about, and those are the ones worth reading verbatim.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if !errors.As(err, &e) {
		return err
	}
	var kind error
	switch e.Number {
	case errUniqueViolation, errUniqueIndex:
		kind = runtime.ErrUniqueViolation
	case errConstraintViolation:
		// ONE number for two conditions, which PostgreSQL and MySQL both split.
		// The message says which — "FOREIGN KEY constraint" or "CHECK
		// constraint" — and the distinction matters to a caller: a foreign key
		// is a reference that is not there, a check is a value the table
		// refuses, and the 409 you return for one is not the 422 you return for
		// the other. Reading the message is the only way the server offers.
		kind = runtime.ErrForeignKeyViolation
		if strings.Contains(e.kindHint, "CHECK") {
			kind = runtime.ErrCheckViolation
		}
	case errNotNullViolation:
		kind = runtime.ErrNotNullViolation
	case errDeadlock:
		kind = runtime.ErrDeadlock
	case errLockTimeout:
		// A NOWAIT read finding the row locked, and a lock wait that ran out,
		// arrive as the same number. PostgreSQL reports 55P03 for both too.
		kind = runtime.ErrLockNotAvailable
	default:
		// No mapping for ErrExclusionViolation: SQL Server has no exclusion
		// constraint, and compile/msddl refuses the declaration. None for
		// ErrSerializationFailure either — a snapshot-isolation conflict is
		// 3960, which is a different retry story and is left unclassified until
		// storm has a reason to make the distinction.
		return err
	}
	return &runtime.ConstraintError{
		Kind:       kind,
		Constraint: e.Constraint,
		Table:      e.Table,
		Err:        err,
	}
}

// code returns a server error's number, or 0.
func code(err error) int32 {
	var e *Error
	if errors.As(err, &e) {
		return e.Number
	}
	return 0
}
