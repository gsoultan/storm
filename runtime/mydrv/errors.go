package mydrv

// Server errors, carried with their code.
//
// The wire gives a numeric code, a five-character SQLSTATE and a message. Only
// the code is stable: the message is localised and reworded between versions,
// so anything that DECIDES on an error — a retry, a protocol fallback, storm's
// own duplicate-key classification — has to read the number.

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/runtime"
)

// Error is a server error packet.
type Error struct {
	Code     uint16
	SQLState string
	Message  string
}

// Error is assembled rather than formatted: everything under runtime/ is on a
// path the generated code calls, and the gate that forbids formatting there
// does not make an exception for the failure branch.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("mysql ")
	b.WriteString(strconv.FormatUint(uint64(e.Code), 10))
	if e.SQLState != "" {
		b.WriteString(" (")
		b.WriteString(e.SQLState)
		b.WriteByte(')')
	}
	b.WriteString(": ")
	b.WriteString(e.Message)
	return b.String()
}

// MySQL error codes this driver acts on.
const (
	// erUnsupportedPS is "This command is not supported in the prepared
	// statement protocol yet".
	erUnsupportedPS = 1295
	// erQueryInterrupted is what a killed statement returns.
	erQueryInterrupted = 1317
	// erDupEntry is a unique-constraint violation, and erDupEntryWithKey is the
	// same thing reported with the index's name.
	erDupEntry        = 1062
	erDupEntryWithKey = 1586
	// erNoReferencedRow / erRowIsReferenced are foreign-key violations.
	erNoReferencedRow  = 1216
	erRowIsReferenced  = 1217
	erNoReferencedRow2 = 1452
	erRowIsReferenced2 = 1451
	// erBadNull is a NOT NULL violation.
	erBadNull = 1048
	// erCheckConstraint is a CHECK violation. The two servers picked different
	// codes for the same thing.
	erCheckConstraint      = 3819
	erCheckConstraintMaria = 4025
	// erLockDeadlock is InnoDB rolling back the loser of a deadlock.
	erLockDeadlock = 1213
	// erLockWaitTimeout is innodb_lock_wait_timeout expiring, and erLockNowait
	// is a NOWAIT read that would have had to wait.
	erLockWaitTimeout = 1205
	erLockNowait      = 3572
)

// parseError decodes an ERR packet. p starts at the 0xff marker.
//
// The layout is: marker, two-byte code, then — only when the client asked for
// protocol 41, which this driver always does — a '#' and a five-character
// SQLSTATE, then the message. Reading the message from a fixed offset without
// checking the '#' is how a driver ends up reporting "#42000Unknown..." .
func parseError(p []byte) error {
	if len(p) < 3 {
		return errors.New("mydrv: truncated error packet")
	}
	e := &Error{Code: uint16(p[1]) | uint16(p[2])<<8}
	rest := p[3:]
	if len(rest) >= 6 && rest[0] == '#' {
		e.SQLState = string(rest[1:6])
		rest = rest[6:]
	}
	e.Message = string(rest)
	return e
}

// classify turns a server error into one storm's callers can switch on.
//
// This is the ONLY place a MySQL error code is read, for the reason pgxdrv's
// classify gives: the Executor port exists so driver knowledge cannot reach the
// rest of the tree, and "compare 1062 in a handler" is exactly that knowledge
// leaking through it. The vocabulary is runtime's, shared with PostgreSQL, so
// generated code that handles a unique violation handles it on either engine.
//
// Anything unrecognised is returned UNCHANGED. A wrapper that renamed every
// error would hide the ones storm has no opinion about, and those are the ones
// worth reading verbatim.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if !errors.As(err, &e) {
		return err
	}
	var kind error
	switch e.Code {
	case erDupEntry, erDupEntryWithKey:
		kind = runtime.ErrUniqueViolation
	case erNoReferencedRow, erRowIsReferenced, erNoReferencedRow2, erRowIsReferenced2:
		kind = runtime.ErrForeignKeyViolation
	case erBadNull:
		kind = runtime.ErrNotNullViolation
	case erCheckConstraint, erCheckConstraintMaria:
		kind = runtime.ErrCheckViolation
	case erLockDeadlock:
		kind = runtime.ErrDeadlock
	case erLockWaitTimeout, erLockNowait:
		// Both mean the lock was not obtained — one after waiting, one because
		// NOWAIT said not to. PostgreSQL reports 55P03 for both cases too.
		kind = runtime.ErrLockNotAvailable
	default:
		// No mapping for ErrExclusionViolation: MySQL has no exclusion
		// constraint. None for ErrSerializationFailure either — MySQL surfaces
		// a serialization conflict AS a deadlock, and claiming otherwise would
		// invent a distinction the server does not make.
		return err
	}
	return &runtime.ConstraintError{
		Kind:       kind,
		Constraint: constraintName(e),
		Err:        err,
	}
}

// constraintName digs the constraint out of the message, because the server
// does not send it as a field.
//
// Best effort, and empty when it does not fit — which is honest, since
// ConstraintError already documents that any of the three may be empty. What is
// matched is an identifier the server quotes back from the DDL, not translated
// prose, so it survives a server running with a non-English lc_messages. The
// two servers quote with different characters for the same error, which is why
// both are tried.
func constraintName(e *Error) string {
	switch e.Code {
	case erDupEntry, erDupEntryWithKey:
		// "Duplicate entry 'x' for key 'tbl.idx'" — the key is the LAST quoted
		// run, because the offending VALUE is quoted first.
		name := lastQuoted(e.Message, '\'')
		// MySQL 8 qualifies it as table.index; MariaDB does not.
		if k := strings.LastIndexByte(name, '.'); k >= 0 {
			name = name[k+1:]
		}
		return name
	case erCheckConstraint, erCheckConstraintMaria:
		// MySQL: "Check constraint 'x' is violated."
		// MariaDB: "CONSTRAINT `x` failed for `db`.`tbl`"
		if n := firstQuoted(e.Message, '\''); n != "" {
			return n
		}
		return firstQuoted(e.Message, '`')
	default:
		// "... (`db`.`tbl`, CONSTRAINT `fk` FOREIGN KEY ...)" — the table is
		// quoted first, so the marker is what picks out the constraint.
		const marker = "CONSTRAINT `"
		if i := strings.Index(e.Message, marker); i >= 0 {
			return firstQuoted(e.Message[i+len(marker)-1:], '`')
		}
	}
	return ""
}

// firstQuoted returns the first run between two q characters, or "".
func firstQuoted(s string, q byte) string {
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

// lastQuoted returns the last run between two q characters, or "".
func lastQuoted(s string, q byte) string {
	j := strings.LastIndexByte(s, q)
	if j <= 0 {
		return ""
	}
	i := strings.LastIndexByte(s[:j], q)
	if i < 0 {
		return ""
	}
	return s[i+1 : j]
}

// code returns a server error's code, or zero.
func code(err error) uint16 {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}
