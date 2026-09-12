package mydrv

// Binding a list as a JSON document.
//
// The statement text is safe because values are bound, never interpolated. For
// a list that safety moves one layer in: the values ARE the document, so an
// unescaped quote ends the string early and the rest of a caller's value
// becomes JSON syntax. These tests are about that boundary as much as about
// the encoding.

import (
	"strings"
	"testing"
	"time"
)

func jsonOf(t *testing.T, a any) string {
	t.Helper()
	b, ok := appendJSONList(nil, a)
	if !ok {
		t.Fatalf("%T was not recognised as a list", a)
	}
	return string(b)
}

func TestListsEncodeAsJSONArrays(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{[]int64{1, -2, 9223372036854775807}, `[1,-2,9223372036854775807]`},
		{[]int32{1, 2}, `[1,2]`},
		{[]int16{1, 2}, `[1,2]`},
		{[]int{3}, `[3]`},
		{[]float64{1.5, -0.25}, `[1.5,-0.25]`},
		{[]string{"a", "b"}, `["a","b"]`},
		{[]int64{}, `[]`},
		// Bytes cannot travel in a JSON document as themselves - it is text - so
		// they go as hex and the SQL side unpacks them with UNHEX.
		{[][16]byte{{0xde, 0xad}}, `["DEAD` + strings.Repeat("00", 14) + `"]`},
		{[][]byte{{0x00, 0x0f, 0xff}}, `["000FFF"]`},
	} {
		if got := jsonOf(t, tc.in); got != tc.want {
			t.Errorf("%T -> %s, want %s", tc.in, got, tc.want)
		}
	}
}

// Uppercase hex, because that is what MySQL's HEX() produces and what a reader
// comparing a bound document against a server-side expression will see.
func TestHexIsUppercase(t *testing.T) {
	if got := jsonOf(t, [][]byte{{0xab, 0xcd, 0xef}}); got != `["ABCDEF"]` {
		t.Errorf("hex = %s", got)
	}
}

// The one that is a security property rather than a formatting one.
func TestAStringCannotEscapeItsJSONString(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{`plain`, `["plain"]`},
		{`say "hi"`, `["say \"hi\""]`},
		{`back\slash`, `["back\\slash"]`},
		{"line\nbreak", `["line\nbreak"]`},
		{"carriage\rreturn", `["carriage\rreturn"]`},
		{"tab\there", `["tab\there"]`},
		{"bell\x07here", `["bell\u0007here"]`},
		// The payload someone would actually try: close the string, close the
		// element, and append a second one.
		{`x","injected`, `["x\",\"injected"]`},
		// Multi-byte UTF-8 is copied through: JSON strings are UTF-8 and MySQL's
		// parser reads them as such.
		{"h\u00e9llo \u4e16\u754c", "[\"h\u00e9llo \u4e16\u754c\"]"},
	} {
		if got := jsonOf(t, []string{tc.in}); got != tc.want {
			t.Errorf("%q -> %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestTimesEncodeAsMySQLLiterals(t *testing.T) {
	got := jsonOf(t, []time.Time{time.Date(2026, 9, 12, 1, 2, 3, 456789000, time.UTC)})
	if got != `["2026-09-12 01:02:03.456789"]` {
		t.Errorf("time = %s", got)
	}
}

// A declared In predicate stores its list in a slot and binds the slot's
// ADDRESS, so what reaches the binder is *[]T. Before this was handled, every
// fetch plan failed with "no binding for *[][16]uint8".
func TestAPointerToAListIsUnwrapped(t *testing.T) {
	ids := [][16]byte{{1}}
	names := []string{"a"}
	nums := []int64{7}
	for _, in := range []any{&ids, &names, &nums} {
		if _, ok := appendJSONList(nil, deref(in)); !ok {
			t.Errorf("%T was not unwrapped to a list", in)
		}
	}
	// A nil pointer is an absence, not a panic.
	var nilIDs *[][16]byte
	if got := deref(nilIDs); got != nil {
		t.Errorf("a nil list pointer became %#v, want nil", got)
	}
}

// Anything else must fall through to the scalar binder, which names the type.
// Reporting a non-list as an empty array would silently match no rows.
func TestANonListIsNotEncodedAsOne(t *testing.T) {
	for _, in := range []any{int64(1), "s", []uint32{1}, nil} {
		if _, ok := appendJSONList(nil, in); ok {
			t.Errorf("%T was encoded as a list", in)
		}
	}
	if _, err := appendBind(nil, []uint32{1}); err == nil {
		t.Error("an unsupported list type was accepted")
	} else if !strings.Contains(err.Error(), "uint32") {
		t.Errorf("the refusal does not name the type: %v", err)
	}
}

// The whole list is ONE bound value, which is the property ADR-0010 chose this
// form for: the statement's shape cannot depend on how many keys the caller
// happens to have.
func TestAListIsOneBoundValueWhateverItsLength(t *testing.T) {
	short, err := appendBind(nil, []int64{1})
	if err != nil {
		t.Fatal(err)
	}
	long, err := appendBind(nil, []int64{1, 2, 3, 4, 5, 6, 7, 8})
	if err != nil {
		t.Fatal(err)
	}
	// Both are a single length-encoded string: one leading length byte at these
	// sizes, then the document.
	if short[0] != byte(len(short)-1) {
		t.Errorf("the short list is not one length-encoded value: %v", short)
	}
	if long[0] != byte(len(long)-1) {
		t.Errorf("the long list is not one length-encoded value: %v", long)
	}
	if ty, _ := bindType([]int64{1}); ty != typeVarString {
		t.Errorf("a list binds as type 0x%02x, want a string", ty)
	}
}
