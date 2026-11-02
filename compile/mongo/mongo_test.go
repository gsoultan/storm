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
	if Field(composite, "a") != "a" {
		t.Error("a composite key has no _id mapping")
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
	for _, s := range []string{
		FindHead("c", fs) + NoFilter + FindSort + `{"_id":1}` + FindTail,
		ExistsHead("c") + NoFilter + ExistsTail,
		CountHead("c") + NoFilter + CountTail,
		DeleteHead("c") + NoFilter + DeleteTail,
		UpdateHead("c", u) + NoFilter + UpdateTail,
		InsertOne("c", u, fs), UpdateOne("c", u, fs), DeleteOne("c"), GetOne("c", fs),
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
	for _, name := range []string{schema.TypeInterval, schema.TypeInet, schema.TypeJSONB, schema.TypeTSVector, schema.TypeTstzRange} {
		if _, ok := BSONType(col(name, false, "")); ok {
			t.Errorf("%s must be refused", name)
		}
	}
	if _, ok := BSONType(&schema.Column{Type: schema.Type{Name: schema.TypeText, Array: true}}); ok {
		t.Error("arrays are refused for now")
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
