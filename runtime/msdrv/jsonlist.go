package msdrv

import (
	"strconv"
	"time"
)

// Binding a whole key list as ONE parameter.
//
// SQL Server has no array parameter, so `IN (@p1, @p2, @p3)` would make the
// statement's text a function of how many values the caller passed — a shape
// key built from request data rather than from the program, which is storm
// being an interpreter with extra steps. ADR-0010 settled it: one bound JSON
// document, unpacked server-side. compile/mssql writes the OPENJSON that reads
// it; this writes the document.
//
// One difference from runtime/mydrv's version, and it is a simplification. A
// uuid cannot travel through MySQL's JSON as itself — the column is BINARY(16)
// and JSON is text — so mydrv sends hex and the statement calls UNHEX. Here the
// column is UNIQUEIDENTIFIER and OPENJSON converts the canonical text form
// directly, so the key goes as what it is and no expression wraps the indexed
// column.
//
// Written by hand rather than through encoding/json: this is the bind path, it
// runs once per query, and the values are integers, strings, uuids and times
// rather than arbitrary structures. Reflection for that would be the one
// reflective path in a tree that has none.

// jsonList renders a slice of keys as a JSON array, or reports that the value
// is not a list.
func jsonList(a any) (string, bool) {
	switch v := a.(type) {
	case []int64:
		return numbers(len(v), func(i int) int64 { return v[i] }), true
	case []int32:
		return numbers(len(v), func(i int) int64 { return int64(v[i]) }), true
	case []int16:
		return numbers(len(v), func(i int) int64 { return int64(v[i]) }), true
	case []int:
		return numbers(len(v), func(i int) int64 { return int64(v[i]) }), true
	case []string:
		return texts(len(v), func(i int) string { return v[i] }), true
	case [][16]byte:
		return texts(len(v), func(i int) string { return uuidText(v[i]) }), true
	case [][]byte:
		// A binary key. Base64 is what OPENJSON's varbinary conversion reads,
		// which keeps the comparison in the column's own type rather than
		// wrapping it in a conversion the index cannot be used through.
		return texts(len(v), func(i int) string { return base64Std(v[i]) }), true
	case []time.Time:
		return texts(len(v), func(i int) string {
			return v[i].UTC().Format("2006-01-02T15:04:05.9999999Z")
		}), true
	}
	return "", false
}

func numbers(n int, at func(int) int64) string {
	b := make([]byte, 0, n*4+2)
	b = append(b, '[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendInt(b, at(i), 10)
	}
	return string(append(b, ']'))
}

func texts(n int, at func(int) string) string {
	b := make([]byte, 0, n*40+2)
	b = append(b, '[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendJSONString(b, at(i))
	}
	return string(append(b, ']'))
}

// appendJSONString quotes a string for JSON.
//
// The escapes are the mandatory ones: the quote, the backslash, and everything
// below 0x20. A control character left unescaped is not a cosmetic problem —
// OPENJSON refuses the whole document, so one bad key loses every row the
// loader was fetching.
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			b = append(b, '\\', '"')
		case c == '\\':
			b = append(b, '\\', '\\')
		case c == '\n':
			b = append(b, '\\', 'n')
		case c == '\r':
			b = append(b, '\\', 'r')
		case c == '\t':
			b = append(b, '\\', 't')
		case c < 0x20:
			const hex = "0123456789abcdef"
			b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
		default:
			b = append(b, c)
		}
	}
	return append(b, '"')
}

// uuidText renders the canonical hyphenated form, which is what
// OPENJSON … WITH (k uniqueidentifier) parses.
func uuidText(u [16]byte) string {
	const hex = "0123456789abcdef"
	var b [36]byte
	j := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			b[j] = '-'
			j++
		}
		b[j] = hex[u[i]>>4]
		b[j+1] = hex[u[i]&0xF]
		j += 2
	}
	return string(b[:])
}

// base64Std encodes without importing encoding/base64, which would be the only
// use of it in this package and pulls in more than three lines of table lookup.
func base64Std(src []byte) string {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	n := (len(src) + 2) / 3 * 4
	out := make([]byte, n)
	di, si := 0, 0
	for len(src)-si >= 3 {
		v := uint(src[si])<<16 | uint(src[si+1])<<8 | uint(src[si+2])
		out[di] = alpha[v>>18&0x3F]
		out[di+1] = alpha[v>>12&0x3F]
		out[di+2] = alpha[v>>6&0x3F]
		out[di+3] = alpha[v&0x3F]
		si += 3
		di += 4
	}
	switch len(src) - si {
	case 1:
		v := uint(src[si]) << 16
		out[di] = alpha[v>>18&0x3F]
		out[di+1] = alpha[v>>12&0x3F]
		out[di+2] = '='
		out[di+3] = '='
	case 2:
		v := uint(src[si])<<16 | uint(src[si+1])<<8
		out[di] = alpha[v>>18&0x3F]
		out[di+1] = alpha[v>>12&0x3F]
		out[di+2] = alpha[v>>6&0x3F]
		out[di+3] = '='
	}
	return string(out)
}
