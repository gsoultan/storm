package codegen

import (
	"fmt"

	"github.com/gsoultan/raorm/schema"
)

// kind is the shape of a column's Go representation, which decides both the
// decoder and which operators make sense on it.
type kind int

const (
	kindUnsupported kind = iota
	kindBool
	kindInt2
	kindInt4
	kindInt8
	kindFloat4
	kindFloat8
	kindText
	kindBytes
	kindUUID
	kindTimestamptz
)

func goKind(c *schema.Column) kind {
	if c.Type.Array {
		return kindUnsupported // arrays land in M2's follow-up
	}
	switch c.Type.Name {
	case schema.TypeBool:
		return kindBool
	case schema.TypeInt2:
		return kindInt2
	case schema.TypeInt4:
		return kindInt4
	case schema.TypeInt8:
		return kindInt8
	case schema.TypeFloat4:
		return kindFloat4
	case schema.TypeFloat8:
		return kindFloat8
	case schema.TypeText, schema.TypeVarchar:
		return kindText
	case schema.TypeBytea:
		return kindBytes
	case schema.TypeUUID:
		return kindUUID
	case schema.TypeTimestamptz:
		return kindTimestamptz
	}
	if c.Type.Enum {
		return kindText // an enum arrives on the wire as its label
	}
	return kindUnsupported
}

// baseGoType is the non-nullable Go type: what a predicate takes.
func baseGoType(c *schema.Column) string {
	switch goKind(c) {
	case kindBool:
		return "bool"
	case kindInt2:
		return "int16"
	case kindInt4:
		return "int32"
	case kindInt8:
		return "int64"
	case kindFloat4:
		return "float32"
	case kindFloat8:
		return "float64"
	case kindText:
		return "string"
	case kindBytes:
		return "[]byte"
	case kindUUID:
		return "[16]byte"
	case kindTimestamptz:
		return "time.Time"
	}
	return "any"
}

// goType is what the Row field holds: Null[T] when the column is nullable,
// because a pointer would cost an allocation per non-nil field per row.
func goType(c *schema.Column) string {
	base := baseGoType(c)
	if c.NotNull || goKind(c) == kindBytes {
		return base
	}
	return "runtime.Null[" + base + "]"
}

// isNullable mirrors goType's test: a column whose Row field is Null[T].
func isNullable(c *schema.Column) bool {
	return !c.NotNull && goKind(c) != kindBytes
}

// decodeExpr renders one column's decode line.
func decodeExpr(c *schema.Column, i int) string {
	f := exportName(c.Name)
	k := goKind(c)

	if k == kindBytes {
		return fmt.Sprintf("r.%s = runtime.Bytes(rv[%d])", f, i)
	}
	if k == kindText {
		if c.NotNull {
			return fmt.Sprintf("r.%s = sl.Str(rv[%d])", f, i)
		}
		return fmt.Sprintf("r.%s = runtime.NullText(rv[%d], sl)", f, i)
	}

	dec := map[kind]string{
		kindBool:        "runtime.Bool",
		kindInt2:        "runtime.Int2",
		kindInt4:        "runtime.Int4",
		kindInt8:        "runtime.Int8",
		kindFloat4:      "runtime.Float4",
		kindFloat8:      "runtime.Float8",
		kindUUID:        "runtime.UUID",
		kindTimestamptz: "runtime.Timestamptz",
	}[k]

	if c.NotNull {
		if k == kindUUID {
			return fmt.Sprintf("copy(r.%s[:], rv[%d])", f, i)
		}
		return fmt.Sprintf("r.%s = %s(rv[%d])", f, dec, i)
	}
	return fmt.Sprintf("r.%s = runtime.Nullable(rv[%d], %s)", f, i, dec)
}

// opApplies decides whether an operator is legal on a column, so the generated
// API only ever offers predicates that make sense. Ordering on a uuid or a
// LIKE on an integer are compile errors rather than runtime surprises.
func opApplies(op string, k kind, c *schema.Column) bool {
	switch op {
	case "In":
		// `= ANY($1)` binds a whole list to ONE placeholder, so list length
		// never changes the statement — the property relation loading needs.
		switch k {
		case kindUUID, kindText, kindInt2, kindInt4, kindInt8:
			return true
		}
		return false
	case "IsNull", "IsNotNull":
		return !c.NotNull
	case "Like":
		return k == kindText
	case "Gt", "Gte", "Lt", "Lte":
		switch k {
		case kindInt2, kindInt4, kindInt8, kindFloat4, kindFloat8, kindText, kindTimestamptz:
			return true
		}
		return false
	case "Eq", "NotEq":
		return k != kindBytes
	}
	return false
}

// inApplies reports whether a column supports IN, and so needs an array slot.
func inApplies(c colInfo) bool { return opApplies("In", c.kind, c.col) }
