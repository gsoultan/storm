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

	// KExists wraps the preceding arity entries in a correlated EXISTS whose
	// header — table, alias, correlation — the back end supplies per relation
	// through Lowering.Exists. The child predicates inside were lowered by the
	// SAME stack walk as everything else: a filtered semi-join is ordinary
	// tokens plus one wrapper, so statement identity, caching, and cross-
	// boundary placeholder numbering all come from machinery that already
	// exists. The operator field carries the relation id.
	KExists
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

// MaxCols is how many columns a token can address in ONE table.
//
// Col is ten bits, so a token can carry 1024 ids — but a composed statement
// splits that space: the parent's columns below ChildColBase and a wrapped
// child's above it. A table wider than the split is therefore not merely
// unusual, it is WRONG in a composer: a parent column at 550 and a child
// column at 38 both address 550, and the lowering routes on the boundary
// alone, so the parent's predicate would be built from the child package's
// fragment table. Silently, and with the wrong rows.
//
// Any table can be a composer's parent or child — every foreign key generates
// one — so the ceiling is the half, not the whole. Generated code checks
// against this rather than guessing, and a table with more filterable columns
// is a generation error, never a truncation.
const MaxCols = ChildColBase

// MakeLeaf builds a predicate token.
func MakeLeaf(op, col uint32) Tok { return Tok(KLeaf<<28 | op<<22 | col<<12) }

// MakeOrder builds an ORDER BY term.
func MakeOrder(dir, col uint32) Tok { return Tok(KOrder<<28 | dir<<22 | col<<12) }

// MakeCol builds a bare column reference for a row comparison.
func MakeCol(col uint32) Tok { return Tok(KCol<<28 | col<<12) }

// MakeRowCmp builds a row comparison over the preceding arity column tokens.
func MakeRowCmp(op, arity uint32) Tok { return Tok(KRowCmp<<28 | op<<22 | arity&0xfff) }

// MakeExists wraps the preceding arity entries in relation rel's EXISTS.
func MakeExists(rel, arity uint32) Tok { return Tok(KExists<<28 | rel<<22 | arity&0xfff) }

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
//
// It is BOUNDED. Shapes are supposed to come from code structure — that is
// the whole thesis, and `storm lint` budgets them at generate time — but a
// call site can derive structure from request data (n optional filters is up
// to 2ⁿ shapes, user-chosen sort columns multiply it again), and then this
// map is keyed by what the caller sent. A cache with no ceiling is a leak
// that profiles as "memory grows with traffic", so past ShapeCap the whole
// map is dropped and refills from the shapes still in use.
//
// Dropping rather than evicting is deliberate. Eviction needs per-entry usage
// tracking, which means a write on the READ path — a shared cache line dirtied
// by every hit on every core, which is precisely what the warm path exists to
// avoid. A bulk drop costs one allocation on the cold path, keeps the ceiling
// exact, and self-heals: whatever is genuinely hot recompiles once and is
// resident again. A cache that is flushing is a call site that outgrew its
// review, and Flushes reports it.
type TreeCache struct {
	last atomic.Pointer[hotTree]

	mu      sync.RWMutex
	entries map[uint64][]*treeEntry
	n       int
	flushes int64
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

// shapeCap is the ceiling every cache shares, read on the cold path only.
//
// 1024 distinct structures per table is far past what a reviewed call site
// produces — `storm lint` fails a plan long before this — and it bounds a
// cache at roughly a megabyte, so a program that never abuses the builder
// never learns this exists.
var shapeCap atomic.Int64

func init() { shapeCap.Store(1024) }

// SetShapeCap changes the ceiling for every cache in the process. A value of
// zero or less means unbounded, which is the old behaviour and is honest to
// offer: a program whose shapes provably come from code, and which would
// rather never recompile, can say so.
//
// It is read when a new shape is compiled, so it takes effect immediately and
// costs nothing on the warm path.
func SetShapeCap(n int) { shapeCap.Store(int64(n)) }

// ShapeCap reports the current ceiling.
func ShapeCap() int { return int(shapeCap.Load()) }

// Flushes reports how many times this cache hit the ceiling and dropped.
//
// Nonzero means shapes are being minted from data rather than from code. The
// fix is at the call site — a filter that should be one shape with a
// nullable parameter rather than 2ⁿ — not a bigger cap.
func (c *TreeCache) Flushes() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return int(c.flushes)
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
	c.n++
	if max := int(shapeCap.Load()); max > 0 && c.n > max {
		// Drop everything, including what was just inserted: `last` still
		// holds it, so this call site keeps its statement and the rest of the
		// working set pays one recompile each. Statements already handed out
		// stay valid — callers hold the pointer, not the map.
		c.entries = make(map[uint64][]*treeEntry)
		c.n = 0
		c.flushes++
	}
	c.last.Store(&hotTree{toks: owned, stmt: st})
	return st
}

// Shapes reports how many distinct structures have compiled. `storm lint` uses
// it to catch a query builder that mints a statement per request.
func (c *TreeCache) Shapes() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.n
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

	// Exists opens relation rel's correlated subquery, correlation included:
	// `EXISTS (SELECT 1 FROM "posts" AS "_storm_e" WHERE "_storm_e"."fk" =
	// "parent"."pk"`. The splicer appends " AND ", the wrapped predicates,
	// and the close. Nil when the context has no filtered relations.
	Exists func(rel uint32) string

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
	return spliceTree(prefix, "", toks, lw, suffix, 0)
}

// SpliceTreeWhere is SpliceTree with a DECLARED predicate ANDed in front of the
// dynamic ones.
//
// A join can declare a filter — "only fulfilled orders" — and a caller's own
// predicates compose with it rather than replacing it. ANDed rather than
// merged, so no call site can widen what the declaration narrowed; that is the
// whole reason to declare it there instead of at every call site.
func SpliceTreeWhere(prefix, declared string, toks []Tok, lw Lowering, suffix string) *Stmt {
	return spliceTree(prefix, declared, toks, lw, suffix, 0)
}

// SpliceTreeFrom is SpliceTree with the first `reserved` ordinals already
// spent, so numbering starts at reserved+1.
//
// A declared aggregation can carry parameters inside a FILTER — "the last
// thirty days", which is relative to when the query runs and so cannot be a
// declaration-time literal. Those live in the statement's PREFIX, which is
// fixed at generate time and can therefore spell $1..$k itself. The dynamic
// predicates then continue from k+1.
//
// Reserving rather than scanning the prefix for `$` on purpose: a
// declaration-time literal may legitimately contain one — `'$5.00'` is a text
// constant, not a placeholder — and a scanner would number it. The prefix
// knows its own count; the splicer only needs to be told.
func SpliceTreeFrom(prefix string, toks []Tok, lw Lowering, suffix string, reserved int) *Stmt {
	return spliceTree(prefix, "", toks, lw, suffix, reserved)
}

func spliceTree(prefix, declared string, toks []Tok, lw Lowering, suffix string, reserved int) *Stmt {
	frag, ord2, ob := lw.Frag, lw.Order, lw.OB
	var stack []string
	ord := reserved

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

		case KExists:
			n := t.Arity()
			if n > len(stack) {
				n = len(stack)
			}
			open := ""
			if lw.Exists != nil {
				open = lw.Exists(t.Op())
			}
			inner := join(stack[len(stack)-n:], " AND ")
			stack = stack[:len(stack)-n]
			if n == 0 {
				// No child predicates: the bare existence check, closed as-is.
				stack = append(stack, open+")")
				continue
			}
			stack = append(stack, open+" AND "+inner+")")

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
	var streamErr error
	switch {
	case len(stack) == 0:
		// No call-site predicates. The declared one, if any, still applies.
		if declared != "" {
			sql += " WHERE " + declared
		}
	case len(stack) == 1:
		if stack[0] != "" {
			w := unwrapOuter(stack[0])
			if declared != "" {
				// Parenthesised: the caller's predicate may be a disjunction,
				// and `declared AND a OR b` is not what either party meant.
				w = declared + " AND (" + w + ")"
			}
			sql += " WHERE " + w
		} else if declared != "" {
			sql += " WHERE " + declared
		}
	default:
		// The stream did not reduce. Every entry is kept, ANDed, so the
		// placeholders already numbered still appear and NArg stays honest —
		// but the statement is marked, because the old behaviour was to drop
		// the WHERE clause entirely and return every row. A filter must never
		// fail open.
		w := join(stack, " AND ")
		if declared != "" {
			w = declared + " AND " + w
		}
		sql += " WHERE " + w
		streamErr = ErrMalformedStream
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
		if suffix[i] != placeholderSigil {
			continue
		}
		// A sigil ALREADY carrying an ordinal is not the splicer's to number.
		//
		// Two things put one there. A declared parameter used in a HAVING is
		// spelled $1 at generate time, because it lives in the fixed text and
		// its number is known then. And a declaration-time text literal may
		// simply contain a dollar — `'$5.00'` is a price, not a placeholder.
		// Numbering either produced `$31` out of `$1` and a statement the
		// server could not type.
		if i+1 < len(suffix) && suffix[i+1] >= '0' && suffix[i+1] <= '9' {
			continue
		}
		ord++
		b.WriteString(itoa(ord))
	}
	return &Stmt{SQL: b.String(), NArg: ord, Err: streamErr}
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

// ChildColBase is where a wrapped child's column ids start inside a combined
// stream. The context package rebases child tokens past it; each side's FragOf
// stays blind to the other, and the composite lowering routes on the range.
// Tok's column field is ten bits, so both halves keep 512 columns.
const ChildColBase = 512

// OffsetCols rebases the column id of every leaf and cursor token by delta,
// appending to dst. Group, order and exists tokens carry no column and pass
// through unchanged.
func OffsetCols(dst, toks []Tok, delta uint32) []Tok {
	for _, t := range toks {
		switch t.Kind() {
		case KLeaf:
			dst = append(dst, MakeLeaf(t.Op(), t.Col()+delta))
		case KCol:
			dst = append(dst, MakeCol(t.Col()+delta))
		default:
			dst = append(dst, t)
		}
	}
	return dst
}
