package mgdrv

import "testing"

// FuzzReply: no reply document a server sends panics the code that reads its
// verdict, its fields and a dotted path into it.
func FuzzReply(f *testing.F) {
	f.Add([]byte{0x0d, 0, 0, 0, 0x10, 'o', 'k', 0, 1, 0, 0, 0, 0})
	f.Add([]byte{0x05, 0, 0, 0, 0})
	f.Add([]byte{0x14, 0, 0, 0, 0x03, 'a', 0, 0x0b, 0, 0, 0, 0x08, 'b', 0, 1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, doc []byte) {
		_ = elements(doc, func(t byte, name string, v []byte) bool {
			_, _ = asInt64(t, v)
			_, _ = asString(t, v)
			return true
		})
		_ = okOf(doc)
		_ = intField(doc, "n")
		_ = strField(doc, "errmsg")
		_, _, _ = path(doc, "a.b.c")
	})
}

// FuzzTemplate: no command text panics the template compiler. The text is
// generated, not a caller's, but the parser is hand-written and the generator
// that feeds it changes.
func FuzzTemplate(f *testing.F) {
	for _, s := range []string{
		`{"find":"t","filter":{"a":{"$arg":1}},"projection":{"a":1}}`,
		`{"aggregate":"t","pipeline":[{"$count":"n"}],"cursor":{}}`,
		`{"insert":"t","documents":[{"x":{"$numberLong":"9"},"y":1.5,"z":null,"w":true}]}`,
		`{"a":"A\"",`, `{`, `{"x":[`, `{"find":"t","x":-1e999}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		tm, err := compile(text)
		if err != nil {
			return
		}
		args := make([]any, tm.nargs)
		_, _ = tm.encode(nil, args)
	})
}
