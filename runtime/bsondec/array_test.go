package bsondec

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

// doc builds a BSON document (or array) body from typed elements.
func doc(els ...[]byte) []byte {
	var body []byte
	for _, e := range els {
		body = append(body, e...)
	}
	b := binary.LittleEndian.AppendUint32(nil, uint32(4+len(body)+1))
	return append(append(b, body...), 0)
}

// el is one element: type, name, value bytes (the value without its type).
func el(name string, v []byte) []byte {
	return append(append(append([]byte{v[0]}, name...), 0), v[1:]...)
}

func TestArray(t *testing.T) {
	arr := append([]byte{TArray}, doc(el("0", str("a")), el("1", str("b")))...)
	got, err := Array(arr, Str)
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatal(got, err)
	}
	empty := append([]byte{TArray}, doc()...)
	if got, err := Array(empty, Str); err != nil || got == nil || len(got) != 0 {
		t.Fatal("an empty array is empty, not NULL", got, err)
	}
	ints := append([]byte{TArray}, doc(el("0", i32(TInt32, 7)), el("1", i64(TInt64, 8)))...)
	if got, err := Array(ints, Int8); err != nil || got[0] != 7 || got[1] != 8 {
		t.Fatal(got, err)
	}
	withNull := append([]byte{TArray}, doc(el("0", []byte{TNull}))...)
	if _, err := Array(withNull, Str); !errors.Is(err, runtime.ErrShape) {
		t.Error("a null element must be refused:", err)
	}
	wrong := append([]byte{TArray}, doc(el("0", i32(TInt32, 1)))...)
	if _, err := Array(wrong, Str); err == nil {
		t.Error("an int element is not text")
	}
	if _, err := Array(str("x"), Str); err == nil {
		t.Error("a string is not an array")
	}
	if _, err := Array([]byte{TArray, 1, 2}, Str); err == nil {
		t.Error("a truncated array")
	}
}

func TestElementsRefusesMalformed(t *testing.T) {
	good := doc(el("a", str("x")))
	for name, b := range map[string][]byte{
		"short":          {1, 2},
		"length":         append([]byte{99, 0, 0, 0}, good[4:]...),
		"no terminator":  append(good[:len(good)-1:len(good)-1], 1),
		"unknown type":   doc([]byte{0x42, 'a', 0}),
		"unnamed":        {6, 0, 0, 0, 2, 0},
		"value overruns": doc([]byte{TInt64, 'a', 0, 1, 2}),
	} {
		if err := Elements(b, func(byte, string, []byte) error { return nil }); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	stop := errors.New("stop")
	if err := Elements(good, func(byte, string, []byte) error { return stop }); err != stop {
		t.Error("fn's error is returned")
	}
}

func TestSize(t *testing.T) {
	for _, c := range []struct {
		t    byte
		b    []byte
		want int
	}{
		{TDouble, make([]byte, 8), 8}, {TDouble, make([]byte, 7), -1},
		{TInt32, make([]byte, 4), 4}, {TDecimal, make([]byte, 16), 16}, {TBool, []byte{1}, 1},
		{TNull, nil, 0}, {0x07, make([]byte, 12), 12},
		{TString, []byte{2, 0, 0, 0, 'a', 0}, 6}, {TString, []byte{1}, -1}, {TString, []byte{99, 0, 0, 0}, -1},
		{TDocument, []byte{5, 0, 0, 0, 0}, 5}, {TBinary, []byte{1, 0, 0, 0, 0, 9}, 6},
		{0x0B, []byte{'a', 0, 'i', 0}, 4}, {0x0B, []byte{'a', 0}, -1}, {0x42, nil, -1},
	} {
		if got := Size(c.t, c.b); got != c.want {
			t.Errorf("Size(0x%02x, %v) = %d, want %d", c.t, c.b, got, c.want)
		}
	}
}

func TestJSON(t *testing.T) {
	inner := doc(el("h", i32(TInt32, 20)))
	arr := doc(el("0", str("x")), el("1", []byte{TBool, 1}), el("2", []byte{TNull}),
		el("3", i64(TInt64, 1<<40)), el("4", i64(TDouble, int64(0x3FF8000000000000))))
	d := doc(el("s", str("q\"\\\n\x01é")), el("o", append([]byte{TDocument}, inner...)),
		el("a", append([]byte{TArray}, arr...)),
		el("t", i64(TDateTime, 0)), el("b", bin(0, []byte{1, 2})), el("id", append([]byte{0x07}, make([]byte, 12)...)),
		el("d", dec(15, 0x3040000000000000)), el("u", []byte{0x06}))
	got, err := JSON(append([]byte{TDocument}, d...))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"s":"q\"\\\u000a\u0001é","o":{"h":20},"a":["x",true,null,1099511627776,1.5],` +
		`"t":"1970-01-01T00:00:00Z","b":"AQI=","id":"000000000000000000000000","d":15,"u":null}`
	if string(got) != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if s, _ := JSON(str("x")); string(s) != `"x"` {
		t.Error(string(s))
	}
	if s, _ := JSON(i64(TDouble, int64(0x7FF0000000000000))); string(s) != `"+Inf"` {
		t.Error("infinity is a string:", string(s))
	}
	if s, _ := JSON(dec(0, 0x7C00000000000000)); string(s) != `"NaN"` {
		t.Error("a decimal NaN:", string(s))
	}
	if s, _ := JSON(append([]byte{TString}, 0xff, 0xff, 0xff, 0xff)); s != nil {
		t.Error("a malformed string")
	}
	for _, b := range [][]byte{nil, {0x42}, {TDocument, 1}, append([]byte{TDocument}, doc(el("x", []byte{0x42}))...),
		bin(0, nil)[:3]} {
		if _, err := JSON(b); err == nil {
			t.Errorf("%v decoded", b)
		}
	}
	bad := append([]byte{TDocument}, doc(el("x", []byte{0x7F}))...) // max key: no JSON for it
	if _, err := JSON(bad); err == nil {
		t.Error("a max key has no JSON form")
	}
	if s, _ := JSON(append([]byte{TString}, 2, 0, 0, 0, 0xff, 0)); string(s) != "\"�\"" {
		t.Error("invalid UTF-8 is replaced:", string(s))
	}
}
