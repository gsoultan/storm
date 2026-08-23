package runtime_test

import (
	"strings"
	"testing"

	"github.com/gsoultan/raorm/runtime"
)

func frag(op, col uint32) runtime.Frag {
	cols := []string{"a", "b", "c"}
	switch op {
	case 1:
		return runtime.Frag{A: cols[col] + " = $"}
	case 2:
		return runtime.Frag{A: cols[col] + " > $"}
	case 3:
		return runtime.Frag{A: cols[col] + " IS NULL"}
	}
	return runtime.Frag{}
}

func leaf(op, col uint32) runtime.Tok { return runtime.MakeLeaf(op, col) }

// ord and ob stand in for what the back end supplies at generate time.
var (
	ord = func(dir, col uint32) string {
		s := "c" + string(rune('0'+col))
		if dir == runtime.Desc {
			return s + " DESC"
		}
		return s + " ASC"
	}
	ob = runtime.Order{Lead: " ORDER BY ", Sep: ", "}
)

// Ordering is part of a statement's identity: two streams differing only in
// their ORDER BY terms must compile to different SQL, or one would serve the
// other's rows in the wrong order.
func TestSpliceTree_OrderIsPartOfTheStatement(t *testing.T) {
	frag := func(op, col uint32) runtime.Frag { return runtime.Frag{A: "x = $"} }
	base := []runtime.Tok{runtime.MakeLeaf(1, 0)}

	asc := runtime.SpliceTree("SEL", append(append([]runtime.Tok{}, base...),
		runtime.MakeOrder(runtime.Asc, 1)), frag, ord, ob, "")
	desc := runtime.SpliceTree("SEL", append(append([]runtime.Tok{}, base...),
		runtime.MakeOrder(runtime.Desc, 1)), frag, ord, ob, "")

	if asc.SQL == desc.SQL {
		t.Fatalf("ASC and DESC produced the same statement: %q", asc.SQL)
	}
	if !strings.Contains(asc.SQL, "ORDER BY c1 ASC") {
		t.Errorf("asc = %q", asc.SQL)
	}
	if !strings.Contains(desc.SQL, "ORDER BY c1 DESC") {
		t.Errorf("desc = %q", desc.SQL)
	}
	// The ordering must not be mistaken for a predicate.
	if strings.Contains(asc.SQL, "WHERE c1") {
		t.Errorf("an order token leaked into the WHERE clause: %q", asc.SQL)
	}
}

// Several terms keep their order and are separated, not concatenated.
func TestSpliceTree_MultipleOrderTerms(t *testing.T) {
	frag := func(op, col uint32) runtime.Frag { return runtime.Frag{} }
	st := runtime.SpliceTree("SEL", []runtime.Tok{
		runtime.MakeOrder(runtime.Desc, 2),
		runtime.MakeOrder(runtime.Asc, 0),
	}, frag, ord, ob, " LIMIT $")
	if want := "SEL ORDER BY c2 DESC, c0 ASC LIMIT $1"; st.SQL != want {
		t.Errorf("got %q, want %q", st.SQL, want)
	}
	if st.NArg != 1 {
		t.Errorf("NArg = %d, want 1 — order terms bind nothing", st.NArg)
	}
}

func TestSpliceTree(t *testing.T) {
	for _, tc := range []struct {
		name string
		toks []runtime.Tok
		want string
		args int
	}{
		{"single leaf", []runtime.Tok{leaf(1, 0)}, "SEL WHERE a = $1 SUF", 1},
		{"and", []runtime.Tok{leaf(1, 0), leaf(2, 1), runtime.MakeGroup(runtime.KAnd, 2)},
			"SEL WHERE a = $1 AND b > $2 SUF", 2},
		{"or", []runtime.Tok{leaf(1, 0), leaf(1, 1), runtime.MakeGroup(runtime.KOr, 2)},
			"SEL WHERE a = $1 OR b = $2 SUF", 2},
		// A AND (B OR C) — the structure a flat per-column mask cannot encode.
		{"and of or", []runtime.Tok{
			leaf(1, 0),
			leaf(1, 1), leaf(2, 2), runtime.MakeGroup(runtime.KOr, 2),
			runtime.MakeGroup(runtime.KAnd, 2)},
			"SEL WHERE a = $1 AND (b = $2 OR c > $3) SUF", 3},
		{"not", []runtime.Tok{leaf(1, 0), runtime.MakeGroup(runtime.KNot, 1)},
			"SEL WHERE NOT (a = $1) SUF", 1},
		// IS NULL consumes no placeholder, so the ordinals must not skip.
		{"no-arg op", []runtime.Tok{leaf(3, 0), leaf(1, 1), runtime.MakeGroup(runtime.KAnd, 2)},
			"SEL WHERE a IS NULL AND b = $1 SUF", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := runtime.SpliceTree("SEL", tc.toks, frag, ord, ob, " SUF")
			if st.SQL != tc.want {
				t.Errorf("\n got  %q\n want %q", st.SQL, tc.want)
			}
			if st.NArg != tc.args {
				t.Errorf("NArg = %d, want %d", st.NArg, tc.args)
			}
		})
	}
}

func TestTreeCacheVerifiesTokens(t *testing.T) {
	c := runtime.NewTreeCache()
	a := []runtime.Tok{leaf(1, 0)}
	b := []runtime.Tok{leaf(2, 1)}

	sa := runtime.SpliceTree("SEL", a, frag, ord, ob, "")
	c.Put(a, sa)

	if got := c.Get(a); got != sa {
		t.Fatal("miss on the structure just stored")
	}
	if got := c.Get(b); got != nil {
		t.Fatal("a different structure must not hit")
	}
	sb := runtime.SpliceTree("SEL", b, frag, ord, ob, "")
	c.Put(b, sb)
	if c.Get(a) != sa || c.Get(b) != sb {
		t.Fatal("entries interfered")
	}
	if c.Shapes() != 2 {
		t.Errorf("Shapes() = %d, want 2", c.Shapes())
	}
	if !strings.Contains(sb.SQL, "b > $1") {
		t.Errorf("unexpected SQL %q", sb.SQL)
	}
}

func TestTreeCacheConcurrent(t *testing.T) {
	c := runtime.NewTreeCache()
	done := make(chan struct{})
	for g := 0; g < 16; g++ {
		go func(g int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 500; i++ {
				toks := []runtime.Tok{leaf(uint32(i%3+1), uint32(g%3))}
				if st := c.Get(toks); st == nil {
					c.Put(toks, runtime.SpliceTree("SEL", toks, frag, ord, ob, ""))
				}
			}
		}(g)
	}
	for g := 0; g < 16; g++ {
		<-done
	}
	if n := c.Shapes(); n < 1 || n > 9 {
		t.Errorf("Shapes() = %d, want between 1 and 9", n)
	}
}
