package mydrv

// Binding a LIST.
//
// PostgreSQL takes a whole key list as one array parameter and unnests it.
// MySQL has no array type, so storm's MySQL lowering binds the list as one JSON
// document and unpacks it with JSON_TABLE (ADR-0010) — which makes turning a Go
// slice into that document the adapter's job, exactly as turning one into a
// PostgreSQL array is pgx's.
//
// Hand-written rather than encoding/json: the types are a closed set (a key is
// an integer, a string, or bytes), the document is built straight into the
// packet buffer, and marshalling by reflection on the hot path of every fetch
// plan is the cost this package exists to avoid.

import (
	"strconv"
	"time"
)

// appendJSONList writes a Go slice as a JSON array.
//
// BYTES GO AS HEX, and the SQL side unpacks them with UNHEX — see
// compile/mysql's jsonKey. A JSON document is text and arbitrary bytes are not
// valid UTF-8, so a uuid cannot travel as itself. Hex rather than base64
// because MySQL's UNHEX is available everywhere and FROM_BASE64 is 5.6+, and
// because a hex string of a fixed width is a fixed-width CHAR column, which is
// what keeps the comparison on the key's index.
func appendJSONList(b []byte, a any) ([]byte, bool) {
	b = append(b, '[')
	switch v := a.(type) {
	case []int64:
		for i, x := range v {
			b = appendSep(b, i)
			b = strconv.AppendInt(b, x, 10)
		}
	case []int32:
		for i, x := range v {
			b = appendSep(b, i)
			b = strconv.AppendInt(b, int64(x), 10)
		}
	case []int16:
		for i, x := range v {
			b = appendSep(b, i)
			b = strconv.AppendInt(b, int64(x), 10)
		}
	case []int:
		for i, x := range v {
			b = appendSep(b, i)
			b = strconv.AppendInt(b, int64(x), 10)
		}
	case []float64:
		for i, x := range v {
			b = appendSep(b, i)
			b = strconv.AppendFloat(b, x, 'g', -1, 64)
		}
	case []string:
		for i, x := range v {
			b = appendSep(b, i)
			b = appendJSONString(b, x)
		}
	case [][16]byte:
		for i, x := range v {
			b = appendSep(b, i)
			b = appendHexString(b, x[:])
		}
	case [][]byte:
		for i, x := range v {
			b = appendSep(b, i)
			b = appendHexString(b, x)
		}
	case []time.Time:
		for i, x := range v {
			b = appendSep(b, i)
			b = append(b, '"')
			b = append(b, x.Format("2006-01-02 15:04:05.000000")...)
			b = append(b, '"')
		}
	default:
		// Not a list this adapter knows. Whether it is a slice at all is not
		// worth a reflect import to find out: appendBind's refusal names the
		// type, which is what a caller needs either way.
		return nil, false
	}
	return append(b, ']'), true
}

func appendSep(b []byte, i int) []byte {
	if i > 0 {
		return append(b, ',')
	}
	return b
}

const hexDigits = "0123456789ABCDEF"

func appendHexString(b, v []byte) []byte {
	b = append(b, '"')
	for _, c := range v {
		b = append(b, hexDigits[c>>4], hexDigits[c&0x0f])
	}
	return append(b, '"')
}

// appendJSONString escapes what JSON requires and nothing else.
//
// The escape set is not a style choice: an unescaped quote or backslash ends
// the string early, and the rest of the caller's value becomes JSON syntax.
// That is the injection this whole binding path exists to avoid, moved one
// layer in — the statement text is safe, so the document must be too.
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c == '\n':
			b = append(b, '\\', 'n')
		case c == '\r':
			b = append(b, '\\', 'r')
		case c == '\t':
			b = append(b, '\\', 't')
		case c < 0x20:
			b = append(b, '\\', 'u', '0', '0',
				hexDigits[c>>4], hexDigits[c&0x0f])
		default:
			// Everything else, including multi-byte UTF-8, is copied through:
			// JSON strings are UTF-8 and MySQL's parser reads them as such.
			b = append(b, c)
		}
	}
	return append(b, '"')
}
