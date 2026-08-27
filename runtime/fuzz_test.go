package runtime_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

// The compiler must survive any token stream, including ones a generator would
// never emit.
//
// A malformed stream is not a security hole — tokens carry compiler ids, never
// caller data — but a panic in SpliceTree is a panic inside a query, and a
// library that can be made to panic by a bug in its own generator is not one
// you can run in production. So the property is: never panic, and never emit a
// statement whose placeholder count disagrees with what it reports.
func FuzzSpliceTree(f *testing.F) {
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{0x10, 0x40, 0x00, 0x00, 0x10, 0x80, 0x00, 0x00, 0x20, 0x00, 0x00, 0x02})
	f.Add([]byte{0x40, 0x00, 0x00, 0x00, 0x50, 0x00, 0x00, 0x01})
	f.Add(make([]byte, 64))

	frag := func(op, col uint32) runtime.Frag {
		if op%3 == 0 {
			return runtime.Frag{} // an operator the back end cannot lower
		}
		if op%3 == 1 {
			return runtime.Frag{A: "c = $"}
		}
		return runtime.Frag{A: "c = ANY($", B: ")"}
	}
	lw := runtime.Lowering{
		Frag:       frag,
		Order:      func(dir, col uint32) string { return "c" },
		OB:         runtime.Order{Lead: " ORDER BY ", Sep: ", "},
		Ident:      func(col uint32) string { return "c" },
		RowCmp:     func(op uint32) string { return " > " },
		TupleOpen:  "(",
		TupleSep:   ", ",
		TupleClose: ")",
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		toks := make([]runtime.Tok, 0, len(b)/4)
		for i := 0; i+4 <= len(b); i += 4 {
			toks = append(toks, runtime.Tok(uint32(b[i])<<24|uint32(b[i+1])<<16|
				uint32(b[i+2])<<8|uint32(b[i+3])))
		}
		if len(toks) > 256 {
			toks = toks[:256] // bound the input, not the behaviour
		}

		st := runtime.SpliceTree("SELECT 1 FROM t", toks, lw, " LIMIT $")
		if st == nil {
			t.Fatal("SpliceTree returned nil")
		}
		// NArg must match the placeholders actually written, or a caller binds
		// the wrong number of arguments and the database rejects the statement
		// at run time instead of the generator rejecting it at build time.
		if n := countPlaceholders(st.SQL); n != st.NArg {
			t.Fatalf("NArg = %d but the statement has %d placeholders:\n%s", st.NArg, n, st.SQL)
		}
		// Placeholders must be numbered 1..NArg with no gaps and no repeats.
		for i := 1; i <= st.NArg; i++ {
			if !strings.Contains(st.SQL, "$"+itoa(i)) {
				t.Fatalf("placeholder $%d is missing from:\n%s", i, st.SQL)
			}
		}
	})
}

// countPlaceholders counts `$` followed by a digit.
func countPlaceholders(s string) int {
	n := 0
	for i := 0; i < len(s)-1; i++ {
		if s[i] == '$' && s[i+1] >= '0' && s[i+1] <= '9' {
			n++
		}
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// The mask cache must survive any mask, including ones with more bits set than
// there are columns.
func FuzzSpliceSections(f *testing.F) {
	f.Add(uint64(0), uint8(0))
	f.Add(^uint64(0), uint8(3))

	f.Fuzz(func(t *testing.T, mask uint64, nfrag uint8) {
		if nfrag > 64 {
			nfrag = 64
		}
		var set []runtime.Frag
		for i := 0; i < int(nfrag); i++ {
			if mask&(1<<uint(i%64)) != 0 {
				set = append(set, runtime.Frag{A: "c = $"})
			}
		}
		st := runtime.SpliceSections("UPDATE t SET ", []runtime.Section{
			{Sep: ", ", Frags: set},
			{Lead: " WHERE ", Sep: " AND ", Frags: []runtime.Frag{{A: "id = $"}}},
		}, "")
		if st == nil {
			t.Fatal("nil statement")
		}
		if n := countPlaceholders(st.SQL); n != st.NArg {
			t.Fatalf("NArg = %d but %d placeholders:\n%s", st.NArg, n, st.SQL)
		}
	})
}
