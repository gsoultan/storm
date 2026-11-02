package mgdrv

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
)

func TestCompileReadsTheResultShape(t *testing.T) {
	cases := map[string][]string{
		`{"find":"c","projection":{"_id":1,"a":{"$slice":1},"b":1},"filter":{}}`:                          {"_id", "a", "b"},
		`{"findAndModify":"c","query":{"_id":{"$arg":1}},"update":{},"fields":{"x":1}}`:                   {"x"},
		`{"aggregate":"c","cursor":{},"pipeline":[{"$match":{}},{"$count":"n"}]}`:                         {"n"},
		`{"aggregate":"c","cursor":{},"pipeline":[{"$project":{"p":1,"q":1}},{"$limit":1}]}`:              {"p", "q"},
		` { "find" : "c" , "projection" : { "a" : [1, {"b":"}"}], "z" : 1 } } `:                           {"a", "z"},
		`{"insert":"c","documents":[{"a":"x\"y","b":true,"c":false,"d":null,"e":-1.5e3,"f":9999999999}]}`: nil,
	}
	for text, want := range cases {
		tm, err := compile(text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if strings.Join(tm.cols, ",") != strings.Join(want, ",") {
			t.Errorf("%s: cols %v, want %v", text, tm.cols, want)
		}
	}
	tm, _ := compile(`{"find":"c","filter":{"a":{"$arg":2},"b":{"$in":[{"$arg":1}]},"c":{"$arg" : 3 }}}`)
	if tm.cmd != "find" || tm.nargs != 3 {
		t.Fatalf("%+v", tm)
	}
	// An object that merely starts like a hole is an object.
	tm, _ = compile(`{"find":"c","filter":{"a":{"$arg":1,"x":2}}}`)
	if tm.nargs != 0 {
		t.Error("{$arg, x} is not a hole")
	}
}

func TestCompileRefusesWhatIsNotACommand(t *testing.T) {
	for _, text := range []string{``, `[]`, `{}`, `{"a":}`, `{"a":1`, `{"a":1}x`, `{"a" 1}`, `{"a":[1 2]}`,
		`{"a":"unterminated}`, `{"a":"\x"}`, `{"a":tru}`, `{"a":{"b":1 "c":2}}`, `{1:2}`} {
		if _, err := compile(text); err == nil {
			t.Errorf("%q compiled", text)
		}
	}
}

func TestEncodeBindsEveryArgumentType(t *testing.T) {
	tm, err := compile(`{"insert":"c","documents":[{"v":{"$arg":1}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	s, i, b, f, i16, i32, i64, f32 := "s", 1, true, 1.5, int16(2), int32(3), int64(4), float32(.5)
	u, tt, d, by := [16]byte{9}, time.Unix(1, 0), runtime.Decimal{Unscaled: 1}, []byte{1}
	var nilStr *string
	for _, v := range []any{nil, s, &s, nilStr, b, &b, i, i16, &i16, i32, &i32, i64, &i64, f32, &f32, f, &f,
		tt, &tt, u, &u, by, &by, []byte(nil), d, &d,
		[]string{"a"}, []int64{1}, []int32{1}, []int16{1}, []float64{1}, []float32{1}, []bool{true},
		[][16]byte{u}, []time.Time{tt}, []runtime.Decimal{d},
		(*bool)(nil), (*int16)(nil), (*int32)(nil), (*int64)(nil), (*float32)(nil), (*float64)(nil),
		(*time.Time)(nil), (*[16]byte)(nil), (*[]byte)(nil), (*runtime.Decimal)(nil)} {
		b, at := startDoc(nil)
		b, err := tm.encode(b, []any{v})
		if err != nil {
			t.Fatalf("%T: %v", v, err)
		}
		b = endDoc(b, at)
		if err := elements(b, func(byte, string, []byte) bool { return true }); err != nil {
			t.Fatalf("%T encodes malformed BSON", v)
		}
	}
	buf, _ := startDoc(nil)
	if _, err := tm.encode(buf, []any{struct{}{}}); err == nil {
		t.Error("an unsupported type must be refused")
	}
	if _, err := tm.encode(buf, nil); err == nil {
		t.Error("too few arguments must be refused")
	}
}

func TestCompiledCacheIsBounded(t *testing.T) {
	a, _ := compiled(`{"ping":1}`)
	b, _ := compiled(`{"ping":1}`)
	if a != b {
		t.Error("a compiled command is reused")
	}
	if _, err := compiled(`nope`); err == nil {
		t.Error("garbage compiles")
	}
	for i := 0; i < tmplCap+1; i++ {
		_, _ = compiled(`{"ping":` + string(rune('a'+i%26)) + `}`)
	}
	tmplMu.RLock()
	n := len(tmplCache)
	tmplMu.RUnlock()
	if n > tmplCap {
		t.Errorf("cache holds %d", n)
	}
}

func TestReadingHelpers(t *testing.T) {
	b, at := startDoc(nil)
	b = appendInt32(b, "i", 7)
	b = appendInt64(b, "l", 8)
	b = appendStr(b, "s", "x")
	b = appendBool(b, "t", true)
	b, _ = appendValue(b, "d", 2.0)
	b = appendName(b, tRegex, "re")
	b = append(b, 'a', 0, 'i', 0)
	b = appendName(b, tObjectID, "o")
	b = append(b, make([]byte, 12)...)
	b = appendName(b, tMinKey, "min")
	b = endDoc(b, at)
	if intField(b, "i") != 7 || intField(b, "l") != 8 || intField(b, "d") != 2 || intField(b, "t") != 1 ||
		strField(b, "s") != "x" || intField(b, "nope") != 0 || strField(b, "i") != "" {
		t.Fatal("lookups")
	}
	if _, _, ok := lookup(b, "min"); !ok {
		t.Error("an element after a regex and an ObjectId is found")
	}
	for _, bad := range [][]byte{nil, {5, 0, 0, 0, 1}, {9, 0, 0, 0, tInt32, 'a', 0, 1, 0}, {8, 0, 0, 0, 0x55, 'a', 0, 0}} {
		if elements(bad, func(byte, string, []byte) bool { return true }) == nil {
			t.Errorf("% x walks", bad)
		}
	}
	if collOf("db.c.x") != "c.x" || collOf("c") != "c" {
		t.Error("collOf")
	}
}

func TestErrors(t *testing.T) {
	e := &Error{Code: 11000, Name: "DuplicateKey", Message: "E11000 duplicate key error collection: d.c index: uq_c_a dup key: { a: 1 }"}
	var ce *runtime.ConstraintError
	if err := classify(e); !errors.Is(err, runtime.ErrUniqueViolation) || !errors.As(err, &ce) || ce.Constraint != "uq_c_a" {
		t.Fatal(err)
	}
	if !strings.Contains(e.Error(), "11000 (DuplicateKey)") {
		t.Error(e.Error())
	}
	if err := classify(&Error{Code: 121}); !errors.Is(err, runtime.ErrCheckViolation) {
		t.Error("validation")
	}
	if err := classify(&Error{Code: 112}); !runtime.Retryable(err) {
		t.Error("a write conflict is retryable")
	}
	if err := classify(&Error{Code: 1, Labels: []string{"TransientTransactionError"}}); !runtime.Retryable(err) {
		t.Error("a transient transaction error is retryable")
	}
	if err := classify(&Error{Code: 2}); errors.As(err, &ce) {
		t.Error("anything else is the server's error as it is")
	}
	if indexName("no index here") != "" {
		t.Error("indexName")
	}
}

func TestParseURI(t *testing.T) {
	c, err := ParseURI("mongodb://u:p%40ss@db.example:27019/app?authSource=admin&tls=true&maxPoolSize=4&connectTimeoutMS=500&appName=x&replicaSet=rs0&directConnection=true&authMechanism=SCRAM-SHA-256")
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "db.example:27019" || c.Database != "app" || c.User != "u" || c.Password != "p@ss" ||
		c.AuthSource != "admin" || c.TLS == nil || c.TLS.ServerName != "db.example" || c.MaxConns != 4 ||
		c.DialTimeout != 500*time.Millisecond || c.AppName != "x" {
		t.Fatalf("%+v", c)
	}
	c, _ = ParseURI("mongodb://localhost")
	if c.Addr != "localhost:27017" || c.Database != "test" || c.TLS != nil {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []string{"postgres://x", "mongodb+srv://x", "mongodb://a,b/db", "mongodb://x/?w=1",
		"mongodb://x/?maxPoolSize=0", "mongodb://x/?connectTimeoutMS=x", "mongodb://x/?authMechanism=PLAIN",
		"mongodb://x/?tlsCAFile=/nonexistent", "::"} {
		if _, err := ParseURI(bad); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}
