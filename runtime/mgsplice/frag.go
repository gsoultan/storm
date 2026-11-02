package mgsplice

// Frag is one lowered comparison: A, then the value's hole, then B — or, when
// Arg is false, the whole predicate in A. Generated code builds a table of
// them from compile/mongo at generate time.
type Frag struct {
	A, B string
	Arg  bool
}
