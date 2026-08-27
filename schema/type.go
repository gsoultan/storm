package schema

import "strconv"

// Type is a column type in storm's IR. It is deliberately Postgres-shaped but
// carries no dialect logic: lowering lives in compile/.
type Type struct {
	Name      string // "text", "uuid", "int4", "numeric", "timestamptz", or an enum name
	Size      int    // varchar(n); 0 = unbounded
	Precision int    // numeric(p,s)
	Scale     int
	Array     bool // T[]
	Enum      bool // Name refers to a Schema.Enums entry
}

// Canonical Postgres type names, used by every front end so that a model and
// an introspected database produce the same IR.
const (
	TypeBool        = "bool"
	TypeInt2        = "int2"
	TypeInt4        = "int4"
	TypeInt8        = "int8"
	TypeFloat4      = "float4"
	TypeFloat8      = "float8"
	TypeNumeric     = "numeric"
	TypeText        = "text"
	TypeVarchar     = "varchar"
	TypeBytea       = "bytea"
	TypeUUID        = "uuid"
	TypeTimestamptz = "timestamptz"
	TypeTimestamp   = "timestamp"
	TypeDate        = "date"
	TypeTime        = "time"
	TypeInterval    = "interval"
	TypeJSONB       = "jsonb"
	TypeJSON        = "json"
	TypeInet        = "inet"
	TypeCIDR        = "cidr"
	TypeMacaddr     = "macaddr"
	TypeTSVector    = "tsvector"
	TypeHstore      = "hstore"
)

// SQL renders the type as Postgres DDL.
func (t Type) SQL() string {
	s := t.base()
	if t.Array {
		s += "[]"
	}
	return s
}

func (t Type) base() string {
	switch t.Name {
	case TypeVarchar:
		if t.Size > 0 {
			return "varchar(" + strconv.Itoa(t.Size) + ")"
		}
		return "varchar"
	case TypeNumeric:
		if t.Precision > 0 {
			if t.Scale > 0 {
				return "numeric(" + strconv.Itoa(t.Precision) + "," + strconv.Itoa(t.Scale) + ")"
			}
			return "numeric(" + strconv.Itoa(t.Precision) + ")"
		}
		return "numeric"
	default:
		return t.Name
	}
}

// Equal compares two types for schema-diff purposes.
func (t Type) Equal(o Type) bool {
	return t.Name == o.Name && t.Size == o.Size &&
		t.Precision == o.Precision && t.Scale == o.Scale &&
		t.Array == o.Array
}
