// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package codegenmongo

import "github.com/gsoultan/storm/schema"

// kind is a column's Go representation on this back end: the type a Row field
// and a predicate take, the decoder that reads it, and the handle type that
// offers its operators.
type kind struct {
	goType string // the non-nullable Go type
	dec    string // the runtime/bsondec function, for a scalar
	handle string // the column handle type
	// ordered columns have Gt/Gte/Lt/Lte, text the pattern matches, and
	// listable ones In/NotIn.
	ordered, text, listable bool
	// slice is a type whose nil already means NULL, so its field is not
	// wrapped in runtime.Null — the same rule the SQL back ends follow.
	slice bool
	// array is a BSON array of elem, json a document stored as BSON; neither
	// has a scalar comparison, and each has the operators of its own.
	array, json bool
	elem        string // an array's element Go type
	elemDec     string // and its bsondec function
	// counter columns are integers, with Inc and Add.
	counter bool
}

func kindOf(c *schema.Column) (kind, bool) {
	if c.Type.Array {
		e := *c
		e.Type.Array = false
		ek, ok := kindOf(&e)
		if !ok || ek.slice || ek.json || ek.goType == "runtime.Decimal" {
			return kind{}, false
		}
		return kind{goType: "[]" + ek.goType, handle: arrayHandle(ek.handle), slice: true,
			array: true, elem: ek.goType, elemDec: ek.dec}, true
	}
	if c.Type.Enum {
		return kind{goType: "string", dec: "Str", handle: "TextCol", ordered: true, text: true, listable: true}, true
	}
	switch c.Type.Name {
	case schema.TypeBool:
		return kind{goType: "bool", dec: "Bool", handle: "BoolCol"}, true
	case schema.TypeInt2:
		return kind{goType: "int16", dec: "Int2", handle: "Int2Col", ordered: true, listable: true, counter: true}, true
	case schema.TypeInt4:
		return kind{goType: "int32", dec: "Int4", handle: "Int4Col", ordered: true, listable: true, counter: true}, true
	case schema.TypeInt8:
		return kind{goType: "int64", dec: "Int8", handle: "Int8Col", ordered: true, listable: true, counter: true}, true
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
	case schema.TypeJSON, schema.TypeJSONB:
		return kind{goType: "runtime.JSON", dec: "JSON", handle: "JSONCol", slice: true, json: true}, true
	}
	return kind{}, false
}

func arrayHandle(scalar string) string {
	return scalar[:len(scalar)-len("Col")] + "ArrayCol"
}

// scalar reports a column with the comparison operators.
func (k kind) scalar() bool { return !k.array && !k.json }

// nullable reports whether a column's Row field is runtime.Null[T].
func (k kind) nullable(c *schema.Column) bool { return !c.NotNull && !k.slice }

// fieldType is the Row field's type.
func (k kind) fieldType(c *schema.Column) string {
	if k.nullable(c) {
		return "runtime.Null[" + k.goType + "]"
	}
	return k.goType
}
