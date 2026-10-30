package mydrv

// unboundError is an argument with no wire form. Its own type because it is
// the one statement failure found BEFORE anything is written, so the
// connection is untouched — the pool keeps it, where an I/O failure discards
// it (see Conn.settle).
type unboundError struct{ err error }

func (e *unboundError) Error() string { return e.err.Error() }
