package storm

// Polymorphic associations: one column referring to a row in one of several
// tables.
//
// # Two shapes, and why only one is the default
//
// OneOf is an EXCLUSIVE ARC: one nullable foreign key per variant, plus a CHECK
// that exactly one is set. Referential integrity survives — a subject cannot be
// deleted out from under a row, because the database still enforces every one
// of those keys.
//
// The Rails and GORM answer is a (subject_type, subject_id) pair, which no
// database can constrain: nothing stops subject_id naming a row that does not
// exist or a table that does not either. storm can express that too, but it
// costs an explicit acknowledgement, because "we gave up referential integrity"
// should appear in a diff.
//
// # Arity
//
// Go generics need a fixed count, so the variants are OneOf2 through OneOf8
// rather than a single variadic type. Eight is where a column each stops being
// reasonable; past that the answer is a supertype table, which keeps integrity
// and has no arity limit at the cost of a second insert.
//
// Every OneOfN is zero-sized: the model is a declaration, and the variant types
// are carried in the field types where the walker can read them.

// OneOf2 is an exclusive arc over two variants.
type OneOf2[A, B any] struct {
	a [0]A
	b [0]B
}

// OneOf3 is an exclusive arc over three variants.
type OneOf3[A, B, C any] struct {
	a [0]A
	b [0]B
	c [0]C
}

// OneOf4 is an exclusive arc over four variants.
type OneOf4[A, B, C, D any] struct {
	a [0]A
	b [0]B
	c [0]C
	d [0]D
}

// OneOf5 is an exclusive arc over five variants.
type OneOf5[A, B, C, D, E any] struct {
	a [0]A
	b [0]B
	c [0]C
	d [0]D
	e [0]E
}

// OneOf6 is an exclusive arc over six variants.
type OneOf6[A, B, C, D, E, F any] struct {
	a [0]A
	b [0]B
	c [0]C
	d [0]D
	e [0]E
	f [0]F
}

// OneOf7 is an exclusive arc over seven variants.
type OneOf7[A, B, C, D, E, F, G any] struct {
	a [0]A
	b [0]B
	c [0]C
	d [0]D
	e [0]E
	f [0]F
	g [0]G
}

// OneOf8 is an exclusive arc over eight variants.
type OneOf8[A, B, C, D, E, F, G, H any] struct {
	a [0]A
	b [0]B
	c [0]C
	d [0]D
	e [0]E
	f [0]F
	g [0]G
	h [0]H
}
