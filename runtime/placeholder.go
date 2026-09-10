package runtime

import "strings"

// Placeholder is how a back end spells a bound parameter.
//
// ADR-0010 decided this carrier and deliberately left it unbuilt, because "a
// second implementation nothing executes is what R9 already cost this project
// once". There is a server to run the result against now.
//
// The zero value is PostgreSQL — `$` followed by an ordinal — so every existing
// caller and every generated PostgreSQL package keeps the spelling it had
// without naming it.
type Placeholder struct {
	// Sigil marks a bound parameter. Zero means '$'.
	Sigil byte

	// Bare suppresses the ordinal after the sigil: MySQL's `?`, where position
	// in the statement is the only thing that binds a value to a parameter.
	//
	// This is not a cosmetic difference. With ordinals, the same value can be
	// referenced twice by writing $1 twice; bare, it must be BOUND twice. Any
	// lowering that reuses an ordinal has to be re-thought for a bare back end
	// rather than re-spelled — which is why this is a field on the lowering and
	// not a string substitution.
	Bare bool
}

func (p Placeholder) sigil() byte {
	if p.Sigil == 0 {
		return '$'
	}
	return p.Sigil
}

// write emits one placeholder into b.
func (p Placeholder) write(b *strings.Builder, ord int) {
	b.WriteByte(p.sigil())
	if !p.Bare {
		b.WriteString(itoa(ord))
	}
}

// text is one placeholder as a string, for the fragment path, which builds by
// concatenation rather than into a builder.
func (p Placeholder) text(ord int) string {
	if p.Bare {
		return string(p.sigil())
	}
	return string(p.sigil()) + itoa(ord)
}

// MySQLPlaceholder is the bare `?`.
//
// Exported so a generated MySQL package can name it in the Lowering it builds,
// and so the choice is visible in the emitted code rather than implied by a
// zero value somewhere.
var MySQLPlaceholder = Placeholder{Sigil: '?', Bare: true}
