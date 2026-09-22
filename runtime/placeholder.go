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

	// Prefix goes between the sigil and the ordinal.
	//
	// SQL Server needs one. Its parameters are NAMED — `@p1` — and a name is a
	// T-SQL identifier, which may not begin with a digit, so `@1` is not a
	// parameter spelled tersely; it is a syntax error. The prefix is what makes
	// an ordinal part of a legal name.
	//
	// Empty for PostgreSQL and MySQL, so neither changes.
	Prefix string
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
		b.WriteString(p.Prefix)
		b.WriteString(itoa(ord))
	}
}

// text is one placeholder as a string, for the fragment path, which builds by
// concatenation rather than into a builder.
func (p Placeholder) text(ord int) string {
	if p.Bare {
		return string(p.sigil())
	}
	return string(p.sigil()) + p.Prefix + itoa(ord)
}

// numbered reports whether what follows position i in s is already an ordinal
// this placeholder wrote — `$1` for PostgreSQL, `@p1` for SQL Server — so the
// suffix scanner leaves it alone. s[i] is the sigil.
//
// A prefix is why this is a function rather than a digit test. Scanning `@p1`
// for a digit at i+1 finds `p`, numbers the sigil anyway and emits `@1p1`: a
// statement naming two parameters where the generator wrote one.
func (p Placeholder) numbered(s string, i int) bool {
	j := i + 1 + len(p.Prefix)
	if j >= len(s) || !strings.HasPrefix(s[i+1:], p.Prefix) {
		return false
	}
	return s[j] >= '0' && s[j] <= '9'
}

// MySQLPlaceholder is the bare `?`.
//
// Exported so a generated MySQL package can name it in the Lowering it builds,
// and so the choice is visible in the emitted code rather than implied by a
// zero value somewhere.
var MySQLPlaceholder = Placeholder{Sigil: '?', Bare: true}

// MSSQLPlaceholder is `@p` followed by an ordinal.
//
// Named, and the name is what the TDS parameter declaration binds against, so
// the ordinal is load-bearing in a way MySQL's position is not: the client
// sends `@p1 int, @p2 nvarchar(64)` alongside the text and the server matches
// by NAME. Reusing an ordinal is therefore legal here and binds once, which is
// what lets a row comparison expand into the OR-form SQL Server needs.
var MSSQLPlaceholder = Placeholder{Sigil: '@', Prefix: "p"}

// OraclePlaceholder is `:` followed by an ordinal.
//
// A bind variable named by a number, which is legal here and is not in T-SQL —
// `@1` is a syntax error because an identifier may not start with a digit,
// which is why MSSQLPlaceholder carries a Prefix and this does not. The name is
// what binds, so reusing an ordinal binds once: the property that lets a
// declared parameter appear in two union branches and be passed once.
var OraclePlaceholder = Placeholder{Sigil: ':'}
