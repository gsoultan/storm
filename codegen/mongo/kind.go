package codegenmongo

import "github.com/gsoultan/storm/schema"

// kind is a column's Go representation on this back end: the type a Row field
// and a predicate take, the decoder that reads it, and the handle type that
// offers its operators.
type kind struct {
	goType string // the non-nullable Go type
	dec    string // the runtime/bsondec function
	handle string // the column handle type
	// ordered columns have Gt/Gte/Lt/Lte, text the pattern matches, and
	// listable ones In/NotIn.
	ordered, text, listable bool
	// slice is a type whose nil already means NULL, so its field is not
	// wrapped in runtime.Null — the same rule the SQL back ends follow.
	slice bool
}

func kindOf(c *schema.Column) (kind, bool) {
	if c.Type.Array {
		return kind{}, false
	}
	if c.Type.Enum {
		return kind{goType: "string", dec: "Str", handle: "TextCol", ordered: true, text: true, listable: true}, true
	}
	switch c.Type.Name {
	case schema.TypeBool:
		return kind{goType: "bool", dec: "Bool", handle: "BoolCol"}, true
	case schema.TypeInt2:
		return kind{goType: "int16", dec: "Int2", handle: "Int2Col", ordered: true, listable: true}, true
	case schema.TypeInt4:
		return kind{goType: "int32", dec: "Int4", handle: "Int4Col", ordered: true, listable: true}, true
	case schema.TypeInt8:
		return kind{goType: "int64", dec: "Int8", handle: "Int8Col", ordered: true, listable: true}, true
	case schema.TypeFloat4:
		return kind{goType: "float32", dec: "Float4", handle: "Float4Col", ordered: true, listable: true}, true
	case schema.TypeFloat8:
		return kind{goType: "float64", dec: "Float8", handle: "Float8Col", ordered: true, listable: true}, true
	case schema.TypeText, schema.TypeVarchar:
		return kind{goType: "string", dec: "Str", handle: "TextCol", ordered: true, text: true, listable: true}, true
	case schema.TypeBytea:
		return kind{goType: "[]byte", dec: "Bytes", handle: "BytesCol", slice: true}, true
	case schema.TypeUUID:
		return kind{goType: "[16]byte", dec: "UUID", handle: "UUIDCol", listable: true}, true
	case schema.TypeTimestamptz:
		return kind{goType: "time.Time", dec: "Time", handle: "TimeCol", ordered: true, listable: true}, true
	case schema.TypeDate:
		return kind{goType: "time.Time", dec: "Date", handle: "TimeCol", ordered: true, listable: true}, true
	case schema.TypeNumeric:
		return kind{goType: "runtime.Decimal", dec: "Decimal", handle: "DecimalCol", ordered: true, listable: true}, true
	}
	return kind{}, false
}

// nullable reports whether a column's Row field is runtime.Null[T].
func (k kind) nullable(c *schema.Column) bool { return !c.NotNull && !k.slice }

// fieldType is the Row field's type.
func (k kind) fieldType(c *schema.Column) string {
	if k.nullable(c) {
		return "runtime.Null[" + k.goType + "]"
	}
	return k.goType
}
