package runtime

import (
	"strings"
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

	// KOrder is one ORDER BY term. Order tokens are appended AFTER the
	// predicate tree, so the same stream is the key for both — an ordering is
	// part of a statement's identity exactly as a predicate is, and two queries
	// that differ only in ordering must not share a compiled statement.
	KOrder

	// KCol is a bare column reference, pushed to be consumed by KRowCmp.
	KCol

	// KRowCmp compares a whole row against a bound tuple: (a, b) > ($1, $2).
	// Arity is how many preceding KCol tokens it takes.
	//
	// A row comparison, not the OR-expansion `a > $1 OR (a = $1 AND b > $2)`
	// that hand-written pagination usually reaches for. The two mean the same
	// thing; only the row comparison lets Postgres walk a multi-column index
	// once instead of planning a disjunction.
	KRowCmp
)

// Sort directions, carried in an order token's operator field.
const (
	Asc uint32 = iota
	Desc
	AscNullsFirst
	DescNullsLast
)

// Row-comparison operators, carried in a KRowCmp token's operator field.
//
// Only strict inequality: a keyset paginator using >= would return the row it
// just showed you.
const (
	CmpGt uint32 = iota
	CmpLt
)

// MaxCols is how many columns a token can address: Col is ten bits wide.
// Generated code checks against this rather than guessing, and a table with
// more filterable columns is a generation error, never a truncation.
const MaxCols = 1 << 10

// MakeLeaf builds a predicate token.
func MakeLeaf(op, col uint32) Tok { return Tok(KLeaf<<28 | op<<22 | col<<12) }

// MakeOrder builds an ORDER BY term.
func MakeOrder(dir, col uint32) Tok { return Tok(KOrder<<28 | dir<<22 | col<<12) }

// MakeCol builds a bare column reference for a row comparison.
func MakeCol(col uint32) Tok { return Tok(KCol<<28 | col<<12) }

// MakeRowCmp builds a row comparison over the preceding arity column tokens.
func MakeRowCmp(op, arity uint32) Tok { return Tok(KRowCmp<<28 | op<<22 | arity&0xfff) }

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
// OrderFn returns the SQL for one ORDER BY term. Generated code supplies it
// from a table built by the back end at generate time.
type OrderFn func(dir, col uint32) string

// Order is the punctuation of an ORDER BY clause, chosen by the back end.
type Order struct{ Lead, Sep string }

// Lowering is every piece of back-end text a read statement needs. It is a
// struct rather than six parameters because the splicer is the one place they
// all meet, and a growing parameter list is how a dialect assumption sneaks in
// unnamed.
//
// Generated code fills it from compile/pgsql at build time. The runtime chooses
// none of it.
type Lowering struct {
	Frag  FragFn
	Order OrderFn
	OB    Order

	// Ident renders a bare column reference, for the left side of a row
	// comparison.
	Ident func(col uint32) string

	// RowCmp renders a row-comparison operator, e.g. " > ".
	RowCmp func(op uint32) string

	// TupleOpen, TupleSep and TupleClose punctuate both sides of a row
	// comparison.
	TupleOpen, TupleSep, TupleClose string
}

// SpliceTree assembles a read statement.
//
// The token stream carries predicates first and ORDER BY terms after, and both
// are part of the key: two queries differing only in ordering are different
// statements, and sharing one would serve the wrong rows in the wrong order.
func SpliceTree(prefix string, toks []Tok, lw Lowering, suffix string) *Stmt {
	frag, ord2, ob := lw.Frag, lw.Order, lw.OB
	var stack []string
	ord := 0

	orderAt := len(toks)
	for i, t := range toks {
		if t.Kind() == KOrder {
			orderAt = i
			break
		}
	}
	orderToks := toks[orderAt:]
	toks = toks[:orderAt]

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

		case KCol:
			stack = append(stack, lw.Ident(t.Col()))

		case KRowCmp:
			n := t.Arity()
			if n > len(stack) {
				n = len(stack)
			}
			cols := stack[len(stack)-n:]
			var b strings.Builder
			b.WriteString(lw.TupleOpen)
			b.WriteString(join(cols, lw.TupleSep))
			b.WriteString(lw.TupleClose)
			b.WriteString(lw.RowCmp(t.Op()))
			b.WriteString(lw.TupleOpen)
			for i := 0; i < n; i++ {
				if i > 0 {
					b.WriteString(lw.TupleSep)
				}
				ord++
				b.WriteByte(placeholderSigil)
				b.WriteString(itoa(ord))
			}
			b.WriteString(lw.TupleClose)
			stack = append(stack[:len(stack)-n], b.String())
		}
	}

	sql := prefix
	if len(stack) == 1 && stack[0] != "" {
		sql += " WHERE " + unwrapOuter(stack[0])
	}
	for i, t := range orderToks {
		if i == 0 {
			sql += ob.Lead
		} else {
			sql += ob.Sep
		}
		sql += ord2(t.Op(), t.Col())
	}
	// Number every placeholder the suffix carries, in order.
	//
	// The previous version took a trailingArgs count, appended that many
	// ordinals after the suffix and joined them with ", $". That spells
	// `LIMIT $1` correctly and spells `LIMIT $ OFFSET $1, $2` for anything
	// else. The suffix already says how many placeholders it has, so the count
	// was a second source of truth that could disagree with it — and did.
	var b strings.Builder
	b.Grow(len(sql) + len(suffix) + 8)
	b.WriteString(sql)
	for i := 0; i < len(suffix); i++ {
		b.WriteByte(suffix[i])
		if suffix[i] == placeholderSigil {
			ord++
			b.WriteString(itoa(ord))
		}
	}
	return &Stmt{SQL: b.String(), NArg: ord}
}

// takesArg reports whether a fragment ends in a placeholder needing an ordinal.
// IS NULL and IS NOT NULL do not.
//
// placeholderSigil marks where an ordinal goes. See the seam note below.
const placeholderSigil = '$'

// KNOWN SEAM GAP, deliberate. This assumes the back end's placeholder is `$`
// followed by an ordinal, which is Postgres and MSSQL but not MySQL's bare `?`
// or Oracle's `:name`. P1b moved the read path's SQL text into compile/pgsql
// and stopped there: the right carrier for placeholder policy is not knowable
// from one back end, and inventing one now would be guessing. M9 decides it,
// with two implementations in hand. Until then this is the one Postgres
// assumption left inside runtime/, and it is written down rather than hidden.
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

// SpliceOrder inserts ORDER BY terms into a lowered statement at its marker.
//
// Greatest-n-per-group statements carry their ordering inside a window clause
// or a lateral subquery, not at the end, so the ordering cannot simply be
// appended. The back end emits the statement with a marker where the clause
// goes and the terms are spliced in here — which keeps every keyword in
// compile/ and leaves the runtime doing nothing but concatenation.
func SpliceOrder(stmt string, terms []string, lead, sep string) string {
	i := strings.Index(stmt, OrderMarker)
	if i < 0 {
		return stmt
	}
	var b strings.Builder
	b.Grow(len(stmt) + len(terms)*16)
	b.WriteString(stmt[:i])
	if len(terms) > 0 {
		b.WriteString(lead)
		b.WriteString(join(terms, sep))
	}
	b.WriteString(stmt[i+len(OrderMarker):])
	return b.String()
}

// OrderMarker is where SpliceOrder puts the ORDER BY clause. It is not SQL and
// never reaches a database.
const OrderMarker = "\x00order\x00"
