package mgexpr

import "github.com/gsoultan/storm/schema"

// node is one parsed expression, already type-checked.
type node struct {
	// kind: 'c' column, 'l' literal, 'f' function call, 'a' arithmetic,
	// 'm' comparison, '&' AND, '|' OR, '!' NOT, 'z' IS NULL, 'k' LIKE.
	kind byte
	// op is the arithmetic or comparison operator, or the function's name.
	op  string
	col *schema.Column
	// lit is a literal's JSON; str is a string literal's value.
	lit string
	str string
	// neg is IS NOT NULL and NOT LIKE.
	neg  bool
	kids []*node
	// cls is the value's kind: "int", "num" (a fractional literal),
	// "float", "decimal", "text", "str" (an untyped string literal), "enum
	// <name>", "bool", "null", or a column type compared only for
	// equality ("uuid", "timestamptz", "date", "bytea").
	cls string
	// nums are the numeric kinds an arithmetic value was made of.
	nums uint8
}

const (
	numInt uint8 = 1 << iota
	numFrac
	numFloat
	numDecimal
)
