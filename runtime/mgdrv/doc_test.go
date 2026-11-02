package mgdrv

import (
	"testing"

	"github.com/gsoultan/storm/runtime"
)

// mkdoc encodes a one-field command document whose field v is bound from
// value, through the same path an argument takes.
func bind(t *testing.T, v any) []byte {
	t.Helper()
	b, at := startDoc(nil)
	b, err := appendValue(b, "v", v)
	if err != nil {
		t.Fatal(err)
	}
	return endDoc(b, at)
}

func TestJSONBindsAsBSON(t *testing.T) {
	got := bind(t, runtime.JSON(`{"s":"x","n":1,"big":9999999999,"f":1.5,"e":1e3,"b":true,"z":null,"a":[1,{"k":false}],"$k":"v"}`))
	_, v, _ := lookup(got, "v")
	want := []struct {
		name string
		t    byte
	}{{"s", tString}, {"n", tInt32}, {"big", tInt64}, {"f", tDouble}, {"e", tDouble}, {"b", tBool}, {"z", tNull}, {"a", tArray}, {"$k", tString}}
	for _, w := range want {
		if typ, _, ok := lookup(v, w.name); !ok || typ != w.t {
			t.Errorf("%s: type 0x%02x %v", w.name, typ, ok)
		}
	}
	_, a, _ := lookup(v, "a")
	if typ, el, _ := lookup(a, "1"); typ != tDocument {
		t.Error("an object inside an array")
	} else if typ, _, _ := lookup(el, "k"); typ != tBool {
		t.Error("its member")
	}
	for _, j := range []runtime.JSON{nil, {}} {
		if typ, _, _ := lookup(bind(t, j), "v"); typ != tNull {
			t.Errorf("%q binds null", j)
		}
	}
	var nilp *runtime.JSON
	if typ, _, _ := lookup(bind(t, nilp), "v"); typ != tNull {
		t.Error("a nil *JSON binds null")
	}
	ptr := runtime.JSON(`[]`)
	if typ, _, _ := lookup(bind(t, &ptr), "v"); typ != tArray {
		t.Error("a *JSON binds its value")
	}
	deep := ""
	for range 102 {
		deep += "["
	}
	for _, bad := range []string{`{`, `1 2`, `{"a" 1}`, `[1,]`, `{1:2}`, `{"a\u0000":1}`, `1e999`, deep, `{"a":}`} {
		b, at := startDoc(nil)
		if _, err := appendValue(b, "v", runtime.JSON(bad)); err == nil {
			t.Errorf("%q bound", bad)
		}
		_ = at
	}
}

func TestNumberLongIsAnInt64(t *testing.T) {
	tm, err := compile(`{"insert":"c","documents":[{"n":{"$numberLong":"5"},"m":{"$numberLong":"x"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	b, at := startDoc(nil)
	b, _ = tm.encode(b, nil)
	b = endDoc(b, at)
	_, docs, _ := lookup(b, "documents")
	_, d0, _ := lookup(docs, "0")
	if typ, v, _ := lookup(d0, "n"); typ != tInt64 || intField(d0, "n") != 5 {
		t.Errorf("n: 0x%02x %v", typ, v)
	}
	if typ, _, _ := lookup(d0, "m"); typ != tDocument {
		t.Error("an unparsable $numberLong stays a document")
	}
}

func TestFieldsNestADottedName(t *testing.T) {
	b, at := startDoc(nil)
	b, err := appendFields(b, []string{"_id.a", "x", "_id.b", "y"}, []any{"k1", int32(1), "k2"})
	if err != nil {
		t.Fatal(err)
	}
	b = endDoc(b, at)
	_, id, ok := lookup(b, "_id")
	if !ok || strField(id, "a") != "k1" || strField(id, "b") != "k2" {
		t.Fatalf("the key subdocument: %v", id)
	}
	if intField(b, "x") != 1 {
		t.Error("x")
	}
	if _, _, ok := lookup(b, "y"); ok {
		t.Error("a field with no value is not written")
	}
	if _, err := appendFields(nil, []string{"_id.a"}, []any{struct{}{}}); err == nil {
		t.Error("an unsupported nested value")
	}
	if _, err := appendFields(nil, []string{"x"}, []any{struct{}{}}); err == nil {
		t.Error("an unsupported value")
	}
}

func TestRawValuesReadsADottedColumn(t *testing.T) {
	b, at := startDoc(nil)
	b = appendName(b, tDocument, "_id")
	b, in := startDoc(b)
	b = appendStr(b, "a", "k1")
	b = appendName(b, tNull, "n")
	b = appendName(b, tDocument, "deep")
	b, in2 := startDoc(b)
	b = appendInt32(b, "z", 7)
	b = endDoc(b, in2)
	b = endDoc(b, in)
	b = appendStr(b, "flat", "f")
	b = endDoc(b, at)
	r := &rows{cols: []string{"_id.a", "_id.n", "_id.missing", "flat", "_id.deep.z", "flat.no", "_id.a.no"}}
	r.doc = b
	v := r.RawValues()
	if s, _ := asString(v[0][0], v[0][1:]); s != "k1" {
		t.Errorf("_id.a: %v", v[0])
	}
	if v[1] != nil || v[2] != nil || v[5] != nil || v[6] != nil {
		t.Error("null, missing, and paths through a non-document are NULL")
	}
	if s, _ := asString(v[3][0], v[3][1:]); s != "f" {
		t.Error("flat")
	}
	if n, _ := asInt64(v[4][0], v[4][1:]); n != 7 {
		t.Error("a two-level path")
	}
}
