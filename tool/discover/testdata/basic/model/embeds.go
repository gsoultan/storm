package model

// This file names no storm type at all, yet it is the ONLY evidence that Local
// is a mixin. A scanner that skipped files without a storm import would report
// Local as a table.
type WithLocal struct {
	Local
	Extra string
}

// hidden is an unexported MIXIN: unreachable from a bootstrap and embedded, so
// it is correct code and must not raise an actionable warning.
type hidden struct{ Trace string }

type WithHidden struct {
	hidden
	Extra string
}
