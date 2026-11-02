// Package mgsplice assembles a MongoDB command from a generated package's
// token stream — the document back end's counterpart of runtime.SpliceTree.
//
// The stream, its tokens and the cache that keys statements on it are
// runtime's own, unchanged: a predicate's STRUCTURE is the same whichever back
// end lowers it, and that is what the logical-plan IR promised (ADR-0004). What
// differs is only the punctuation. SQL writes a group infix, `(a AND b)`;
// MongoDB writes it prefix, `{"$and":[a,b]}`. Both are open, members joined by
// a separator, close — so this splicer is the same walk with other text.
//
// Cold path: it runs once per distinct structure and the cache holds the
// result, exactly as SpliceTree's does.
package mgsplice

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/gsoultan/storm/runtime"
)

// Punctuation is the text a back end writes around groups and sort terms.
// compile/mongo supplies it; it is a parameter rather than constants here so
// this package names no back end's spelling.
type Punctuation struct {
	And, Or, Sep, End, Empty string
}

// Splice renders the predicate tokens of toks as one filter document, numbering
// holes from first, and reports how many it used. Order tokens are skipped;
// Sort renders them.
//
// Top-level entries are ANDed: a query's Where calls conjoin. A malformed
// stream — a group whose arity exceeds what precedes it — is an error rather
// than a filter that silently matches more than it should: the SQL splicer
// learned that by fuzzing, and a filter that fails open is the worst bug a
// query builder can have.
func Splice(toks []runtime.Tok, frag func(op, col uint32) Frag, p Punctuation, first int) (string, int, error) {
	var stack []string
	n := first
	for _, t := range toks {
		switch t.Kind() {
		case runtime.KLeaf:
			f := frag(t.Op(), t.Col())
			if !f.Arg {
				stack = append(stack, f.A)
				continue
			}
			stack = append(stack, f.A+`{"$arg":`+strconv.Itoa(n)+`}`+f.B)
			n++
		case runtime.KAnd, runtime.KOr:
			k := t.Arity()
			if k < 1 || k > len(stack) {
				return "", 0, runtime.ErrMalformedStream
			}
			open := p.And
			if t.Kind() == runtime.KOr {
				open = p.Or
			}
			members := stack[len(stack)-k:]
			s := members[0]
			if k > 1 {
				s = open + strings.Join(members, p.Sep) + p.End
			}
			stack = append(stack[:len(stack)-k], s)
		case runtime.KOrder:
		default:
			// Negation, row comparisons and EXISTS are not generated for this
			// back end; meeting one is a generator bug.
			return "", 0, runtime.ErrMalformedStream
		}
	}
	switch len(stack) {
	case 0:
		return p.Empty, 0, nil
	case 1:
		return stack[0], n - first, nil
	}
	return p.And + strings.Join(stack, p.Sep) + p.End, n - first, nil
}

// Sort renders the order tokens of toks as one sort document, or def when
// there are none.
func Sort(toks []runtime.Tok, term func(dir, col uint32) string, open, sep, close, def string) string {
	var terms []string
	for _, t := range toks {
		if t.Kind() == runtime.KOrder {
			terms = append(terms, term(t.Op(), t.Col()))
		}
	}
	if len(terms) == 0 {
		return def
	}
	return open + strings.Join(terms, sep) + close
}

// The text matches bind a REGULAR EXPRESSION, so the caller's text is escaped
// into one here rather than reaching the server as a pattern. A HasPrefix whose
// argument contains `.*` must match those two characters, not everything.

// Prefix is the expression HasPrefix binds. Anchored at the start, which is
// the one form an index on the field can answer.
func Prefix(s string) string { return "^" + regexp.QuoteMeta(s) }

// Suffix is the expression HasSuffix binds.
func Suffix(s string) string { return regexp.QuoteMeta(s) + "$" }

// Contains is the expression Contains binds.
func Contains(s string) string { return regexp.QuoteMeta(s) }

// Like translates a SQL LIKE pattern — % any run, _ one character, everything
// else literal — into the anchored expression that means the same.
func Like(pattern string) string {
	var b strings.Builder
	b.WriteByte('^')
	lit := 0
	flush := func(i int) {
		b.WriteString(regexp.QuoteMeta(pattern[lit:i]))
	}
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '%':
			flush(i)
			b.WriteString("(?s:.*)")
			lit = i + 1
		case '_':
			flush(i)
			b.WriteString("(?s:.)")
			lit = i + 1
		}
	}
	flush(len(pattern))
	b.WriteByte('$')
	return b.String()
}
