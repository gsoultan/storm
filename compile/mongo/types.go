package mongo

import "github.com/gsoultan/storm/schema"

// BSONType is the $jsonSchema bsonType a column is stored as, or ok=false for
// a type this back end does not carry.
//
// The refusals are the PostgreSQL-only types — ranges, network addresses,
// intervals, tsvector, hstore — plus, for now, arrays and json/jsonb. Those
// two have natural BSON forms (an array, an embedded document) and are the
// next thing this back end should gain; they are refused rather than stored
// as text, because text would be a schema nobody could query as what it is.
func BSONType(c *schema.Column) (string, bool) {
	if c.Type.Array {
		return "", false
	}
	if c.Type.Enum {
		return "string", true
	}
	switch c.Type.Name {
	case schema.TypeBool:
		return "bool", true
	case schema.TypeInt2, schema.TypeInt4:
		return "int", true
	case schema.TypeInt8:
		return "long", true
	case schema.TypeFloat4, schema.TypeFloat8:
		return "double", true
	case schema.TypeText, schema.TypeVarchar:
		return "string", true
	case schema.TypeBytea, schema.TypeUUID:
		return "binData", true
	case schema.TypeTimestamptz, schema.TypeDate:
		// A date is stored as midnight UTC: BSON has one temporal type, and a
		// date that drifted with a time zone would change which day it is.
		return "date", true
	case schema.TypeNumeric:
		return "decimal", true
	}
	return "", false
}

// DefaultKind classifies a column default by who computes it.
type DefaultKind uint8

const (
	// DefaultNone is a column with no default.
	DefaultNone DefaultKind = iota
	// DefaultServerTime is now(): the SERVER's clock, written with
	// $currentDate, never the client's.
	DefaultServerTime
	// DefaultUUIDv4 and DefaultUUIDv7 are generated client-side, as on
	// MySQL: MongoDB has no uuid generator a write can call.
	DefaultUUIDv4
	DefaultUUIDv7
	// DefaultLiteral is a constant, written on insert.
	DefaultLiteral
	// DefaultUnsupported is any other expression. SQL text cannot be
	// evaluated by a document store, and guessing at it is refused.
	DefaultUnsupported
)

// ClassifyDefault reports how a column's default is produced on this back end,
// and for a literal, its JSON form.
func ClassifyDefault(c *schema.Column) (DefaultKind, string) {
	d := c.Default
	switch d {
	case "":
		return DefaultNone, ""
	case "now()", "CURRENT_TIMESTAMP", "current_timestamp":
		if c.Type.Name == schema.TypeTimestamptz {
			return DefaultServerTime, ""
		}
		return DefaultUnsupported, ""
	case "gen_random_uuid()":
		return DefaultUUIDv4, ""
	case "uuidv7()":
		return DefaultUUIDv7, ""
	case "true", "false":
		if c.Type.Name == schema.TypeBool {
			return DefaultLiteral, d
		}
		return DefaultUnsupported, ""
	}
	if lit, ok := sqlStringLiteral(d); ok {
		t, _ := BSONType(c)
		if t != "string" {
			return DefaultUnsupported, ""
		}
		return DefaultLiteral, Str(lit)
	}
	if isNumber(d) {
		switch c.Type.Name {
		case schema.TypeInt2, schema.TypeInt4, schema.TypeInt8, schema.TypeFloat4, schema.TypeFloat8:
			return DefaultLiteral, d
		}
	}
	return DefaultUnsupported, ""
}

// sqlStringLiteral unquotes 'it”s' to it's, optionally followed by a cast.
func sqlStringLiteral(s string) (string, bool) {
	if len(s) < 2 || s[0] != '\'' {
		return "", false
	}
	var out []byte
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			out = append(out, s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			out = append(out, '\'')
			i++
			continue
		}
		rest := s[i+1:]
		if rest == "" || (len(rest) > 2 && rest[:2] == "::") {
			return string(out), true
		}
		return "", false
	}
	return "", false
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	i, dot, digits := 0, false, false
	if s[0] == '-' {
		i = 1
	}
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits = true
		case c == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return digits
}
