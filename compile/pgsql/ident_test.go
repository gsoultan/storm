package pgsql_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/compile/pgsql"
)

// Identifiers come from the model, not from a runtime value, so this is not an
// injection boundary — it is a correctness one. A column named with a quote in
// it must still produce a statement that parses, or the generator emits a
// package whose queries fail at first use.
func FuzzIdent(f *testing.F) {
	for _, s := range []string{
		"email", "Order", `a"b`, `"`, `""`, "a\nb", "a b", "", "ünïcode",
		`x"; DROP TABLE users; --`, strings.Repeat(`"`, 100),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		got := pgsql.Ident(name)

		// Always delimited, so a keyword or a space cannot change the parse.
		if len(got) < 2 || got[0] != '"' || got[len(got)-1] != '"' {
			t.Fatalf("Ident(%q) = %q, which is not quoted", name, got)
		}
		// Every quote inside must be doubled: that is what makes the closing
		// quote unambiguous. Count them — an odd number of consecutive quotes
		// anywhere in the body would end the identifier early.
		body := got[1 : len(got)-1]
		for i := 0; i < len(body); i++ {
			if body[i] != '"' {
				continue
			}
			run := 0
			for i < len(body) && body[i] == '"' {
				run++
				i++
			}
			if run%2 != 0 {
				t.Fatalf("Ident(%q) = %q has an odd run of %d quotes — it terminates early",
					name, got, run)
			}
		}
		// And it must round-trip: undoubling the body gives the input back, so
		// no character was lost or invented.
		if back := strings.ReplaceAll(body, `""`, `"`); back != name {
			t.Fatalf("Ident(%q) = %q does not round-trip: got %q back", name, got, back)
		}
	})
}
