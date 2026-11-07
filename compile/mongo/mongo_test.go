// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mongo

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gsoultan/storm/schema"
)

func TestStrIsValidJSONForEveryByte(t *testing.T) {
	for _, s := range []string{"", "plain", `quo"te`, `back\slash`, "\x00\x1f\n\t", "héllo ☃", "\xff\xfe", "<&>"} {
		var back string
		if err := json.Unmarshal([]byte(Str(s)), &back); err != nil {
			t.Fatalf("%q → %s: %v", s, Str(s), err)
		}
		if utf8.ValidString(s) && s != back {
			t.Errorf("%q round-tripped as %q", s, back)
		}
	}
	if Str("<&>") != `"<&>"` {
		t.Errorf("no HTML escaping: the text is a cache key, got %s", Str("<&>"))
	}
}

func TestFieldMapsTheKeyOntoID(t *testing.T) {
	tb := &schema.Table{PrimaryKey: []string{"id"}}
	if Field(tb, "id") != "_id" || Field(tb, "name") != "name" {
		t.Fatal(Field(tb, "id"), Field(tb, "name"))
	}
	composite := &schema.Table{PrimaryKey: []string{"a", "b"}}
	if Field(composite, "a") != "_id.a" || Field(composite, "c") != "c" {
		t.Error("a composite key is the _id subdocument")
	}
	if got := Fields(tb, []string{"id", "x"}); got[0] != "_id" || got[1] != "x" {
		t.Error(got)
	}
}

// Every fragment, with a value spliced in, is one JSON object — and the
// operators whose MongoDB meaning differs from SQL's are lowered to SQL's.
func TestFragsAreWellFormedAndMeanWhatSQLMeans(t *testing.T) {
	for _, op := range Ops() {
		a, b, arg, ok := Frag(op, "f")
		if !ok {
			t.Fatalf("%s: no lowering", op)
		}
		s := a
		if arg {
			s += `"v"` + b
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Errorf("%s: %s is not JSON: %v", op, s, err)
		}
	}
	a, b, _, _ := Frag("NotEq", "f")
	if a+"1"+b != `{"f":{"$nin":[1,null]}}` {
		t.Errorf("NotEq must exclude null, as SQL's <> does: %s", a+"1"+b)
	}
	a, b, _, _ = Frag("NotIn", "f")
	if !strings.Contains(a+"[1]"+b, `"$ne":null`) {
		t.Errorf("NotIn must exclude null: %s", a+"[1]"+b)
	}
	if _, _, _, ok := Frag("Not", "f"); ok {
		t.Error("negation has no faithful lowering and must be absent")
	}
	if _, _, arg, _ := Frag("IsNull", "f"); arg {
		t.Error("IsNull binds nothing")
	}
}

func TestStatementsAreJSON(t *testing.T) {
	fs := []string{"_id", "name"}
	u := UpdateDoc([]Assign{{Op: "$set", Field: "name", Arg: 2}, {Op: "$currentDate", Field: "at"},
		{Op: "$setOnInsert", Field: "n", Lit: "0"}, {Op: "$set", Field: "x", Arg: 3}})
	if u != `{"$set":{"name":{"$arg":2},"x":{"$arg":3}},"$currentDate":{"at":{"$type":"date"}},"$setOnInsert":{"n":0}}` {
		t.Errorf("update doc: %s", u)
	}
	if UpdateDoc(nil) != `{"$set":{}}` {
		t.Error(UpdateDoc(nil))
	}
	one := &schema.Table{Name: "c", PrimaryKey: []string{"id"}}
	two := &schema.Table{Name: "c", PrimaryKey: []string{"tenant", "id"}}
	if Field(two, "tenant") != "_id.tenant" || Field(two, "x") != "x" || Field(one, "id") != "_id" {
		t.Error("key fields")
	}
	if KeyFilter(two, 3) != `{"_id":{"tenant":{"$arg":3},"id":{"$arg":4}}}` {
		t.Error(KeyFilter(two, 3))
	}
	for _, s := range []string{
		FindHead("c", fs) + NoFilter + FindSort + `{"_id":1}` + FindTail,
		ExistsHead("c") + NoFilter + ExistsTail,
		CountHead("c") + NoFilter + CountTail,
		DeleteHead("c") + NoFilter + DeleteTail,
		UpdateHead("c", u) + NoFilter + UpdateTail,
		InsertOne(one, u, fs), UpdateOne(one, u, fs), DeleteOne(one), GetOne(one, fs),
		InsertOne(two, u, fs), UpdateOne(two, u, fs), DeleteOne(two), GetOne(two, fs),
	} {
		var v map[string]any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	if SortTerm("a", true) != `"a":-1` || SortTerm("a", false) != `"a":1` {
		t.Error("sort terms")
	}
}

func TestTypesAndDefaults(t *testing.T) {
	col := func(name string, enum bool, def string) *schema.Column {
		return &schema.Column{Type: schema.Type{Name: name, Enum: enum}, Default: def}
	}
	for name, want := range map[string]string{
		schema.TypeBool: "bool", schema.TypeInt2: "int", schema.TypeInt4: "int", schema.TypeInt8: "long",
		schema.TypeFloat4: "double", schema.TypeFloat8: "double", schema.TypeText: "string",
		schema.TypeVarchar: "string", schema.TypeBytea: "binData", schema.TypeUUID: "binData",
		schema.TypeTimestamptz: "date", schema.TypeDate: "date", schema.TypeNumeric: "decimal",
	} {
		if got, ok := BSONType(col(name, false, "")); !ok || got != want {
			t.Errorf("%s → %s %v", name, got, ok)
		}
	}
	if got, ok := BSONType(col("mood", true, "")); !ok || got != "string" {
		t.Error("an enum is its label")
	}
	for _, name := range []string{schema.TypeInterval, schema.TypeInet, schema.TypeTSVector, schema.TypeTstzRange} {
		if _, ok := BSONType(col(name, false, "")); ok {
			t.Errorf("%s must be refused", name)
		}
	}
	if got, ok := BSONType(col(schema.TypeJSONB, false, "")); !ok || got != "" || !IsJSON(col(schema.TypeJSON, false, "")) {
		t.Error("json is any BSON value")
	}
	arr := func(name string) *schema.Column {
		return &schema.Column{Type: schema.Type{Name: name, Array: true}}
	}
	if got, ok := BSONType(arr(schema.TypeText)); !ok || got != "array" {
		t.Error("a text array is an array")
	}
	if got, _ := ElemType(arr(schema.TypeInt8)); got != "long" {
		t.Error(got)
	}
	for _, name := range []string{schema.TypeJSONB, schema.TypeNumeric, schema.TypeBytea, schema.TypeInet} {
		if _, ok := BSONType(arr(name)); ok {
			t.Errorf("%s[] must be refused", name)
		}
	}
	if _, ok := ElemType(col(schema.TypeText, false, "")); ok {
		t.Error("a scalar has no element type")
	}
	for _, c := range []struct {
		typ, def, lit string
		ok            bool
	}{
		{schema.TypeText, "'{}'", "[]", true},
		{schema.TypeText, "'{}'::text[]", "[]", true},
		{schema.TypeText, "'{a,b}'", `["a","b"]`, true},
		{schema.TypeInt4, "'{1,-2}'", `[1,-2]`, true},
		{schema.TypeBool, "'{t,false}'", `[true,false]`, true},
		{schema.TypeBool, "'{maybe}'", "", false},
		{schema.TypeInt4, "'{x}'", "", false},
		{schema.TypeText, `'{"a b"}'`, "", false},
		{schema.TypeText, "'{a,NULL}'", "", false},
		{schema.TypeText, "'{a,}'", "", false},
		{schema.TypeText, "'[]'", "", false},
		{schema.TypeUUID, "'{x}'", "", false},
	} {
		k, lit := ClassifyDefault(&schema.Column{Type: schema.Type{Name: c.typ, Array: true}, Default: c.def})
		if (k == DefaultLiteral) != c.ok || lit != c.lit {
			t.Errorf("%s[] default %s: %d %q", c.typ, c.def, k, lit)
		}
	}
	for def, want := range map[string]string{
		"'{}'":        "{}",
		"'[]'::jsonb": "[]",
		`'{"a": [1, 2.5, null, true, false, "x"]}'`: `{"a":[1,2.5,null,true,false,"x"]}`,
		"'{bad'":    "",
		"'1 2'":     "",
		`'{"a" 1}'`: "",
		"'[1,'":     "",
		"'{1:2}'":   "",
	} {
		k, lit := ClassifyDefault(col(schema.TypeJSONB, false, def))
		if lit != want || (k == DefaultLiteral) != (want != "") {
			t.Errorf("jsonb default %s: %d %q", def, k, lit)
		}
	}

	cases := []struct {
		typ, def string
		kind     DefaultKind
		lit      string
	}{
		{schema.TypeText, "", DefaultNone, ""},
		{schema.TypeTimestamptz, "now()", DefaultServerTime, ""},
		{schema.TypeTimestamptz, "CURRENT_TIMESTAMP", DefaultServerTime, ""},
		{schema.TypeDate, "now()", DefaultUnsupported, ""},
		{schema.TypeUUID, "gen_random_uuid()", DefaultUUIDv4, ""},
		{schema.TypeUUID, "uuidv7()", DefaultUUIDv7, ""},
		{schema.TypeBool, "true", DefaultLiteral, "true"},
		{schema.TypeText, "true", DefaultUnsupported, ""},
		{schema.TypeText, "'it''s'", DefaultLiteral, `"it's"`},
		{schema.TypeText, "'x'::text", DefaultLiteral, `"x"`},
		{schema.TypeText, "'x' || 'y'", DefaultUnsupported, ""},
		{schema.TypeInt4, "'x'", DefaultUnsupported, ""},
		{schema.TypeInt4, "-12", DefaultLiteral, "-12"},
		{schema.TypeFloat8, "1.5", DefaultLiteral, "1.5"},
		{schema.TypeNumeric, "1.5", DefaultUnsupported, ""},
		{schema.TypeInt4, "1.2.3", DefaultUnsupported, ""},
		{schema.TypeInt4, "-", DefaultUnsupported, ""},
		{schema.TypeText, "lower('X')", DefaultUnsupported, ""},
		{schema.TypeText, "'unterminated", DefaultUnsupported, ""},
	}
	for _, c := range cases {
		k, lit := ClassifyDefault(col(c.typ, false, c.def))
		if k != c.kind || lit != c.lit {
			t.Errorf("%s default %q: got %d %q, want %d %q", c.typ, c.def, k, lit, c.kind, c.lit)
		}
	}
}

func TestDefaultsSQLSpellsDifferently(t *testing.T) {
	col := func(typ, d string) *schema.Column {
		return &schema.Column{Name: "c", Type: schema.Type{Name: typ}, Default: d}
	}
	for _, c := range []struct {
		col  *schema.Column
		kind DefaultKind
		lit  string
	}{
		{col(schema.TypeInt4, "(0)"), DefaultLiteral, "0"},
		{col(schema.TypeInt4, "0::integer"), DefaultLiteral, "0"},
		{col(schema.TypeInt4, "(-1)::int"), DefaultLiteral, "-1"},
		{col(schema.TypeInt8, " ( 5 ) "), DefaultLiteral, `{"$numberLong":"5"}`},
		{col(schema.TypeBool, "TRUE"), DefaultLiteral, "true"},
		{col(schema.TypeBool, "false::boolean"), DefaultLiteral, "false"},
		{col(schema.TypeText, "('x'::text)"), DefaultLiteral, `"x"`},
		{col(schema.TypeText, "NULL"), DefaultNone, ""},
		{col(schema.TypeTimestamptz, "CURRENT_TIMESTAMP"), DefaultServerTime, ""},
		{col(schema.TypeTimestamptz, "transaction_timestamp()"), DefaultServerTime, ""},
		{col(schema.TypeTimestamptz, "now()::date"), DefaultUnsupported, ""},
		{col(schema.TypeInt4, "(1)+(2)"), DefaultUnsupported, ""},
		{col(schema.TypeInt4, "(1))"), DefaultUnsupported, ""},
	} {
		if k, lit := ClassifyDefault(c.col); k != c.kind || lit != c.lit {
			t.Errorf("%q: %v %q", c.col.Default, k, lit)
		}
	}
}
