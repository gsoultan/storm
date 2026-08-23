package runtime

import (
	"sync"
	"sync/atomic"
)

// Predicate structure.
//
// M2's first shape was four bits of operator per column packed into a uint64.
// That cannot represent disjunction: `age > 18 OR age < 5` uses one column
// twice, and `A AND (B OR C)` has a structure no flat mask encodes. So the
// structure is a postfix token stream instead, and the *stream itself* is the
// compiled-statement key.
//
// Tokens carry only compiler-generated ids — kind, operator, column, arity —
// never a caller's value. Two queries with the same structure and different
// values therefore share one statement, which is the whole point.

// Tok is one node. Layout: kind(4) | op(6) | col(10) | arity(12).
type Tok uint32

// Token kinds.
const (
	KLeaf uint32 = iota
	KAnd
	KOr
	KNot
)

// MakeLeaf builds a predicate token.
func MakeLeaf(op, col uint32) Tok { return Tok(KLeaf<<28 | op<<22 | col<<12) }

// MakeGroup builds an AND/OR/NOT token over the previous arity tokens.
func MakeGroup(kind, arity uint32) Tok { return Tok(kind<<28 | arity&0xfff) }

func (t Tok) Kind() uint32 { return uint32(t) >> 28 }
func (t Tok) Op() uint32   { return uint32(t) >> 22 & 0x3f }
func (t Tok) Col() uint32  { return uint32(t) >> 12 & 0x3ff }
func (t Tok) Arity() int   { return int(uint32(t) & 0xfff) }

// TreeCache maps a token stream to a compiled statement.
//
// The key is a hash of the stream, but a hit is confirmed by comparing the
// tokens themselves. A 64-bit hash collision is vanishingly unlikely and would
// return the wrong SQL, which is not a risk an ORM gets to take.
type TreeCache struct {
	last atomic.Pointer[hotTree]

	mu      sync.RWMutex
	entries map[uint64][]*treeEntry
}

type hotTree struct {
	toks []Tok
	stmt *Stmt
}

type treeEntry struct {
	toks []Tok
	stmt *Stmt
}

func NewTreeCache() *TreeCache {
	return &TreeCache{entries: map[uint64][]*treeEntry{}}
}

// HashToks is FNV-1a over the token stream.
func HashToks(toks []Tok) uint64 {
	var h uint64 = 14695981039346656037
	for _, t := range toks {
		for s := 0; s < 32; s += 8 {
			h ^= uint64(uint32(t) >> uint(s) & 0xff)
			h *= 1099511628211
		}
	}
	return h
}

func sameToks(a, b []Tok) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Get returns the compiled statement for a token stream, or nil.
func (c *TreeCache) Get(toks []Tok) *Stmt {
	// A call site almost always reuses one structure, so check that first: a
	// pointer load plus a short compare beats hashing and locking.
	if h := c.last.Load(); h != nil && sameToks(h.toks, toks) {
		return h.stmt
	}
	key := HashToks(toks)
	c.mu.RLock()
	bucket := c.entries[key]
	c.mu.RUnlock()
	for _, e := range bucket {
		if sameToks(e.toks, toks) {
			c.last.Store(&hotTree{toks: e.toks, stmt: e.stmt})
			return e.stmt
		}
	}
	return nil
}

// Put stores a compiled statement, copying the tokens so the caller's buffer
// can be reused.
func (c *TreeCache) Put(toks []Tok, st *Stmt) *Stmt {
	key := HashToks(toks)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries[key] {
		if sameToks(e.toks, toks) {
			return e.stmt // lost a race; both copies are identical
		}
	}
	owned := make([]Tok, len(toks))
	copy(owned, toks)
	c.entries[key] = append(c.entries[key], &treeEntry{toks: owned, stmt: st})
	c.last.Store(&hotTree{toks: owned, stmt: st})
	return st
}

// Shapes reports how many distinct structures have compiled. `raorm lint` uses
// it to catch a query builder that mints a statement per request.
func (c *TreeCache) Shapes() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, b := range c.entries {
		n += len(b)
	}
	return n
}

// FragFn returns the lowered SQL for one leaf. Generated code supplies it.
type FragFn func(op, col uint32) Frag

// SpliceTree renders a postfix token stream to SQL. Cold path: paid once per
// distinct structure for the life of the process, so the string building here
// costs nothing that matters.
func SpliceTree(prefix string, toks []Tok, frag FragFn, suffix string, trailingArgs int) *Stmt {
	var stack []string
	ord := 0

	for _, t := range toks {
		switch t.Kind() {
		case KLeaf:
			f := frag(t.Op(), t.Col())
			if f.A == "" {
				stack = append(stack, "TRUE")
				continue
			}
			s := f.A
			if takesArg(f) {
				ord++
				s += itoa(ord)
			}
			stack = append(stack, s+f.B)

		case KAnd, KOr:
			n := t.Arity()
			if n > len(stack) {
				n = len(stack)
			}
			parts := stack[len(stack)-n:]
			sep := " AND "
			if t.Kind() == KOr {
				sep = " OR "
			}
			joined := "(" + join(parts, sep) + ")"
			stack = append(stack[:len(stack)-n], joined)

		case KNot:
			if len(stack) == 0 {
				continue
			}
			stack[len(stack)-1] = "NOT (" + stack[len(stack)-1] + ")"
		}
	}

	sql := prefix
	if len(stack) == 1 && stack[0] != "" {
		sql += " WHERE " + unwrapOuter(stack[0])
	}
	sql += suffix
	for i := 0; i < trailingArgs; i++ {
		ord++
		sql += itoa(ord)
		if i+1 < trailingArgs {
			sql += ", $"
		}
	}
	return &Stmt{SQL: sql, NArg: ord}
}

// takesArg reports whether a fragment ends in a placeholder needing an ordinal.
// IS NULL and IS NOT NULL do not.
func takesArg(f Frag) bool {
	return len(f.A) > 0 && f.A[len(f.A)-1] == '$'
}

// unwrapOuter drops the parentheses around a single top-level group; `WHERE (a
// AND b)` and `WHERE a AND b` are the same statement and the shorter one reads
// better in an error message.
func unwrapOuter(s string) string {
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return s
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				return s
			}
		}
	}
	return s[1 : len(s)-1]
}

func join(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	n := len(sep) * (len(parts) - 1)
	for _, p := range parts {
		n += len(p)
	}
	b := make([]byte, 0, n)
	for i, p := range parts {
		if i > 0 {
			b = append(b, sep...)
		}
		b = append(b, p...)
	}
	return string(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
