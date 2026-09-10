package codegen

import (
	"strconv"
	"strings"
)

// lit renders SQL as a Go string literal.
//
// codegen emitted every statement as a RAW string literal, delimited by
// backticks. That is exact and readable for PostgreSQL, whose SQL never
// contains one — and impossible for MySQL, which quotes identifiers WITH
// backticks. Go has no escape for a backtick inside a raw literal: the first
// one ends the string, so
//
//	const selectPrefix = `SELECT `id` FROM `users``
//
// is not valid Go, and no MySQL statement could be emitted at all. The blocker
// is not about SQL, which is why reading the lowering never found it.
//
// So the delimiter is chosen by the content rather than assumed. PostgreSQL
// output is unchanged byte for byte, because none of it contains a backtick —
// which is what makes this a fix and not a reformatting of every generated
// file. TestPostgresStillUsesRawLiterals holds that.
func lit(sql string) string {
	if !strings.Contains(sql, "`") {
		return "`" + sql + "`"
	}
	return strconv.Quote(sql)
}
