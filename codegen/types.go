package codegen

import (
	"fmt"

	"github.com/gsoultan/storm/schema"
)

// kind is the shape of a column's Go representation, which decides both the
// decoder and which operators make sense on it.
type kind int

const (
	kindUnsupported kind = iota
	// kindTSVector is a full-text search column: FILTERABLE but not readable.
	// A tsvector is index support, not data — nobody wants one in a Go struct,
	// and decoding one on every read would be pure cost. It gets a column
	// handle with the match operators and is excluded from Row and from writes.
	kindTSVector
	// kindTstzRange is a tstzrange: readable, writable, and the only kind with
	// the range operators.
	kindTstzRange
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
	kindNumeric
	kindJSONB
	kindTextArray
	kindUUIDArray
	kindDate
	kindInterval
	kindTimeOfDay
	kindInet
	kindInt8Array
	kindDecimalArray
)

func goKind(c *schema.Column) kind {
	if c.Type.Array {
		// Only the element types whose decoder is allocation-shaped like the
		// scalar one. A jsonb[] is a different question and stays unsupported
		// rather than half-supported.
		switch c.Type.Name {
		case schema.TypeText, schema.TypeVarchar:
			return kindTextArray
		case schema.TypeUUID:
			return kindUUIDArray
		case schema.TypeInt8:
			return kindInt8Array
		case schema.TypeNumeric:
			return kindDecimalArray
		}
		return kindUnsupported
	}
	switch c.Type.Name {
	case schema.TypeTSVector:
		return kindTSVector
	case schema.TypeTstzRange:
		return kindTstzRange
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
	case schema.TypeNumeric:
		return kindNumeric
	case schema.TypeJSONB:
		return kindJSONB
	case schema.TypeDate:
		return kindDate
	case schema.TypeInterval:
		return kindInterval
	case schema.TypeTime:
		return kindTimeOfDay
	case schema.TypeInet, schema.TypeCIDR:
		return kindInet
	}
	if c.Type.Enum {
		return kindText // an enum arrives on the wire as its label
	}
	return kindUnsupported
}

// baseGoType is the non-nullable Go type: what a predicate takes.
func baseGoType(c *schema.Column) string {
	switch goKind(c) {
	case kindTSVector:
		// The Go type of a tsvector PREDICATE's argument, which is the search
		// term — a string. The column itself has no Go type because it never
		// travels in a Row.
		return "string"
	case kindTstzRange:
		return "runtime.TstzRange"
	case kindTextArray:
		return "[]string"
	case kindUUIDArray:
		return "[][16]byte"
	case kindDate:
		return "time.Time"
	case kindInterval:
		return "runtime.Interval"
	case kindTimeOfDay:
		return "runtime.TimeOfDay"
	case kindInet:
		return "netip.Prefix"
	case kindInt8Array:
		return "[]int64"
	case kindDecimalArray:
		return "[]runtime.Decimal"
	case kindNumeric:
		return "runtime.Decimal"
	case kindJSONB:
		// Raw bytes, not a decoded value. The generator cannot know the shape
		// of a jsonb column — that is the point of the column — so it hands
		// back what the database sent and the caller unmarshals into a type it
		// declared. Decoding into map[string]any here would allocate a map per
		// row for callers who wanted a struct.
		return "runtime.JSON"
	}
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
	if !isNullable(c) {
		return base
	}
	return "runtime.Null[" + base + "]"
}

// MaxNumericPrecision is how many significant digits a runtime.Decimal holds:
// its unscaled value is an int64.
//
// A column declared past this is a GENERATION error rather than a runtime one.
// The database would accept the value and the scan would refuse it, which is a
// production incident triggered by data rather than by deployment — the worst
// kind to debug. Declaring numeric(19,4) is a decision, so it gets an answer at
// the moment it is made.
const MaxNumericPrecision = 18

// checkNumeric rejects a numeric column a Decimal cannot carry.
func checkNumeric(table string, c *schema.Column) error {
	if goKind(c) != kindNumeric || c.Type.Precision == 0 {
		return nil
	}
	if c.Type.Precision > MaxNumericPrecision {
		return fmt.Errorf(
			"codegen: table %s column %s is numeric(%d,%d), but storm.Decimal holds %d "+
				"significant digits — narrow the precision, or declare the column as text "+
				"if it genuinely needs more",
			table, c.Name, c.Type.Precision, c.Type.Scale, MaxNumericPrecision)
	}
	return nil
}

// isNullable mirrors goType's test: a column whose Row field is Null[T].
//
// A slice is not wrapped: nil already means SQL NULL and an empty non-nil slice
// means '{}'. Wrapping would give two ways to say absent and a caller checking
// the wrong one.
func isNullable(c *schema.Column) bool {
	switch goKind(c) {
	case kindBytes, kindTextArray, kindUUIDArray, kindInt8Array, kindDecimalArray:
		return false
	}
	return !c.NotNull
}

// decodeExpr renders one column's decode line.
// decodeExpr emits the scan for one column, using the PostgreSQL family.
func decodeExpr(c *schema.Column, i int) string {
	return decodeExprIn(c, i, decodersFor(DialectPostgres, ""))
}

// decodeExprIn emits the scan for one column against a decoder family.
//
// Which family is a GENERATE-time choice, so the emitted call names its package
// outright and no dialect test survives into the running program. The two
// families share no bytes — MySQL is little-endian and packs its temporal types
// component-wise — so this routes every name through d.q rather than spelling
// one package (ADR-0007).
func decodeExprIn(c *schema.Column, i int, d decoders) string {
	f := exportName(c.Name)
	k := goKind(c)

	if k == kindBytes {
		return fmt.Sprintf("r.%s = "+d.q("Bytes")+"(rv[%d])", f, i)
	}
	if k == kindDate {
		if c.NotNull {
			return fmt.Sprintf("r.%s = "+d.q("Date")+"(rv[%d])", f, i)
		}
		return fmt.Sprintf("r.%s = "+d.q("Nullable")+"(rv[%d], "+d.q("Date")+")", f, i)
	}
	if k == kindInt8Array {
		return fmt.Sprintf("r.%s, decErr = "+d.q("Int8Array")+"(rv[%d])", f, i)
	}
	if k == kindDecimalArray {
		return fmt.Sprintf("r.%s, decErr = "+d.q("DecimalArray")+"(rv[%d])", f, i)
	}
	if k == kindInterval {
		if c.NotNull {
			return fmt.Sprintf("r.%s, decErr = "+d.q("IntervalErr")+"(rv[%d])", f, i)
		}
		return fmt.Sprintf("r.%s, decErr = "+d.q("NullInterval")+"(rv[%d])", f, i)
	}
	if k == kindTimeOfDay {
		if c.NotNull {
			return fmt.Sprintf("r.%s, decErr = "+d.q("TimeOfDayErr")+"(rv[%d])", f, i)
		}
		return fmt.Sprintf("r.%s, decErr = "+d.q("NullTimeOfDay")+"(rv[%d])", f, i)
	}
	if k == kindTstzRange {
		if c.NotNull {
			return fmt.Sprintf("r.%s, decErr = "+d.q("TstzRangeErr")+"(rv[%d])", f, i)
		}
		return fmt.Sprintf("r.%s, decErr = "+d.q("NullTstzRange")+"(rv[%d])", f, i)
	}
	if k == kindInet {
		if c.NotNull {
			return fmt.Sprintf("r.%s, decErr = "+d.q("InetErr")+"(rv[%d])", f, i)
		}
		return fmt.Sprintf("r.%s, decErr = "+d.q("NullInet")+"(rv[%d])", f, i)
	}
	if k == kindTextArray {
		return fmt.Sprintf("r.%s, decErr = "+d.q("TextArray")+"(rv[%d], sl)", f, i)
	}
	if k == kindUUIDArray {
		return fmt.Sprintf("r.%s, decErr = "+d.q("UUIDArray")+"(rv[%d])", f, i)
	}
	if k == kindJSONB {
		if c.NotNull {
			return fmt.Sprintf("r.%s = "+d.q("JSON")+"("+d.q("JSONB")+"(rv[%d], sl))", f, i)
		}
		return fmt.Sprintf("r.%s = "+d.q("NullJSON")+"(rv[%d], sl)", f, i)
	}
	if k == kindNumeric {
		// The error is recorded on the row rather than returned, because a
		// scanner runs per column inside a loop that has no error path. Every
		// terminal checks it before handing the rows back, so a value a
		// Decimal cannot carry never escapes as a plausible zero.
		if c.NotNull {
			return fmt.Sprintf("r.%s, decErr = "+d.q("NumericErr")+"(rv[%d])", f, i)
		}
		return fmt.Sprintf("r.%s, decErr = "+d.q("NullNumeric")+"(rv[%d])", f, i)
	}
	if k == kindText {
		if c.NotNull {
			return fmt.Sprintf("r.%s = sl.Str(rv[%d])", f, i)
		}
		return fmt.Sprintf("r.%s = "+d.q("NullText")+"(rv[%d], sl)", f, i)
	}

	dec := map[kind]string{
		kindBool:        d.q("Bool"),
		kindInt2:        d.q("Int2"),
		kindInt4:        d.q("Int4"),
		kindInt8:        d.q("Int8"),
		kindFloat4:      d.q("Float4"),
		kindFloat8:      d.q("Float8"),
		kindUUID:        d.q("UUID"),
		kindTimestamptz: d.q("Timestamptz"),
	}[k]

	if c.NotNull {
		if k == kindUUID {
			return fmt.Sprintf("copy(r.%s[:], rv[%d])", f, i)
		}
		return fmt.Sprintf("r.%s = %s(rv[%d])", f, dec, i)
	}
	return fmt.Sprintf("r.%s = "+d.q("Nullable")+"(rv[%d], %s)", f, i, dec)
}

// opApplies decides whether an operator is legal on a column, so the generated
// API only ever offers predicates that make sense. Ordering on a uuid or a
// LIKE on an integer are compile errors rather than runtime surprises.
func opApplies(op string, k kind, c *schema.Column) bool {
	switch op {
	case "In", "NotIn":
		// `= ANY($1)` binds a whole list to ONE placeholder, so list length
		// never changes the statement — the property relation loading needs.
		// NotIn is `<> ALL($1)` for the same reason.
		switch k {
		case kindUUID, kindText, kindInt2, kindInt4, kindInt8:
			return true
		}
		return false
	case "IsNull", "IsNotNull":
		return !c.NotNull
	case "Matches", "WebSearch":
		return k == kindTSVector
	case "Overlaps", "ContainsRange", "ContainedBy":
		return k == kindTstzRange
	case "ArrayContains", "ArrayContainedBy", "ArrayOverlaps":
		// The three operators that make an array column queryable. Until they
		// existed, an array round-tripped and could only be tested for NULL:
		// storable, not filterable, which is the shape of gap that sends a
		// caller to raw SQL for something the model already describes.
		//
		// Deliberately NOT equality. `tags = '{a,b}'` is order- and
		// duplicate-sensitive, which almost nobody means; @> and && are the
		// questions people actually have, and they are the ones GIN indexes.
		switch k {
		case kindTextArray, kindUUIDArray, kindInt8Array, kindDecimalArray:
			return true
		}
		return false
	case "Like", "ILike":
		return k == kindText
	case "Gt", "Gte", "Lt", "Lte":
		// A range has no useful < or >: PostgreSQL defines one for sorting, and
		// almost every caller who reaches for it means Overlaps.
		switch k {
		case kindInt2, kindInt4, kindInt8, kindFloat4, kindFloat8, kindText,
			kindTimestamptz, kindNumeric, kindDate, kindTimeOfDay:
			// A time of day has an unambiguous total order — 09:00 really is
			// before 17:00 — which is exactly what a schedule query asks for,
			// and is why it gets the comparisons an interval cannot have.
			return true
		}
		return false
	case "Eq", "NotEq":
		// jsonb equality is whole-document equality, which is almost never
		// what a caller means and is a trap dressed as a feature. Filtering
		// jsonb needs ->> and @>, which the operator set does not have yet;
		// until it does, the only predicates offered are IS [NOT] NULL, so a
		// caller reaches for raw SQL knowingly rather than getting a
		// surprising answer.
		// An array offers no value predicates either: containment and overlap
		// need @> and &&, which the operator set does not have, and equality
		// on an array is order-sensitive in a way almost nobody means.
		// Comparing a tsvector for equality asks whether two documents have
		// identical lexeme vectors, which nobody means; the match operators
		// are the whole reason the column exists.
		switch k {
		case kindTSVector, kindBytes, kindJSONB, kindTextArray, kindUUIDArray, kindInt8Array,
			kindDecimalArray:
			return false
		case kindInterval:
			// Interval equality compares normalised values ('24:00' = '1 day'),
			// which surprises in both directions. Until an operator can say
			// which comparison it means, offering none beats offering a trap.
			return false
		}
		return true
	}
	return false
}

// readable reports whether a column travels in a Row.
//
// A tsvector does not: it is index support, and decoding one on every read
// would be pure cost for a value nobody wants in a Go struct. It is still
// FILTERABLE — that is the whole point of having it — which is why this is a
// separate question from goKind.
func readable(c *schema.Column) bool {
	k := goKind(c)
	return k != kindUnsupported && k != kindTSVector
}
