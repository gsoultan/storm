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
	// erDupEntry is a unique-constraint violation.
	erDupEntry = 1062
	// erNoReferencedRow / erRowIsReferenced are foreign-key violations.
	erNoReferencedRow  = 1216
	erRowIsReferenced  = 1217
	erNoReferencedRow2 = 1452
	erRowIsReferenced2 = 1451
	// erBadNull is a NOT NULL violation.
	erBadNull = 1048
	// erCheckConstraint is a CHECK violation (MySQL 8.0.16+, MariaDB 10.2+).
	erCheckConstraint  = 3819
	erCheckConstraintM = 4025
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

// code returns a server error's code, or zero.
func code(err error) uint16 {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// IsDuplicate reports whether err is a unique-constraint violation.
func IsDuplicate(err error) bool { return code(err) == erDupEntry }

// IsForeignKey reports whether err is a foreign-key violation, in either
// direction: a child pointing at a parent that is not there, or a parent whose
// children still point at it.
func IsForeignKey(err error) bool {
	switch code(err) {
	case erNoReferencedRow, erRowIsReferenced, erNoReferencedRow2, erRowIsReferenced2:
		return true
	}
	return false
}

// IsNotNull reports whether err is a NOT NULL violation.
func IsNotNull(err error) bool { return code(err) == erBadNull }

// IsCheck reports whether err is a CHECK-constraint violation. MySQL and
// MariaDB use different codes for the same thing.
func IsCheck(err error) bool {
	switch code(err) {
	case erCheckConstraint, erCheckConstraintM:
		return true
	}
	return false
}
