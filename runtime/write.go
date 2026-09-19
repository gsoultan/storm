package runtime

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
)

// Write-side statement assembly.
//
// An UPDATE's SET list is chosen by which fields were assigned, and that is a
// shape in the same sense the read path uses the word: the *set* of columns is
// the statement's identity, the values are not. So the same bargain applies —
// compile once per distinct mask, and the warm path does a cache probe and a
// bind, never a string build.
//
// The key here is a plain uint64 bitmask rather than the read path's token
// stream, and that difference matters: masks compare exactly. There is no hash
// and therefore no collision to defend against.

// Section is one run of fragments joined by Sep, introduced by Lead. SET and
// WHERE are both sections; they differ only in their separator.
type Section struct {
	Lead  string
	Sep   string
	Frags []Frag
}

// SpliceSections assembles a write statement, numbering placeholders across
// every section in order. Cold path: paid once per mask for the life of the
// process.
func SpliceSections(prefix string, secs []Section, suffix string) *Stmt {
	return SpliceSectionsWith(prefix, secs, suffix, Placeholder{})
}

// SpliceSectionsWith is SpliceSections for a back end whose placeholder is not
// PostgreSQL's. A generated MySQL package passes runtime.MySQLPlaceholder; the
// zero value is the PostgreSQL spelling, so the plain form above stays exact.
func SpliceSectionsWith(prefix string, secs []Section, suffix string, ph Placeholder) *Stmt {
	var b strings.Builder
	b.WriteString(prefix)
	ord := 0
	for _, s := range secs {
		if len(s.Frags) == 0 {
			continue
		}
		b.WriteString(s.Lead)
		for i, f := range s.Frags {
			if i > 0 {
				b.WriteString(s.Sep)
			}
			if takesArg(f) {
				ord++
				// The fragment ends in the sigil; the back end decides what
				// follows it.
				b.WriteString(f.A[:len(f.A)-1])
				ph.write(&b, ord)
			} else {
				b.WriteString(f.A)
			}
			b.WriteString(f.B)
		}
	}
	b.WriteString(suffix)
	return &Stmt{SQL: b.String(), NArg: ord}
}

// MaskCache maps a dirty mask to a compiled statement.
type MaskCache struct {
	hot atomic.Pointer[maskEntry]

	mu      sync.RWMutex
	entries map[uint64]*maskEntry
}

// maskEntry pairs a mask with its statement so the warm path can publish both
// in ONE atomic store.
//
// They were two atomics — a uint64 and a *Stmt — written in sequence. Two
// goroutines warming different masks could interleave those writes and leave
// the mask from one beside the statement from the other, after which a hit
// handed back an UPDATE compiled for a different column set and the caller
// bound its arguments against those placeholders. Every access was atomic, so
// it was not a data race and -race could not see it; only the pairing was
// unsynchronised. Interning the pair and publishing the pointer makes the
// mismatch unrepresentable.
type maskEntry struct {
	mask uint64
	stmt *Stmt
}

func NewMaskCache() *MaskCache { return &MaskCache{entries: map[uint64]*maskEntry{}} }

// Get returns the statement for a mask, or nil. Allocation-free — the entry is
// interned by Put — and on the common case of one mask repeated it is one
// atomic load and a compare.
func (c *MaskCache) Get(mask uint64) *Stmt {
	if h := c.hot.Load(); h != nil && h.mask == mask {
		return h.stmt
	}
	c.mu.RLock()
	e := c.entries[mask]
	c.mu.RUnlock()
	if e == nil {
		return nil
	}
	c.hot.Store(e)
	return e.stmt
}

// Put interns a statement. Two goroutines compiling the same mask is harmless;
// the first one interned wins and both return the same pointer, so a shape
// never has two slab hints racing.
func (c *MaskCache) Put(mask uint64, st *Stmt) *Stmt {
	c.mu.Lock()
	if prev, ok := c.entries[mask]; ok {
		c.mu.Unlock()
		c.hot.Store(prev)
		return prev.stmt
	}
	e := &maskEntry{mask: mask, stmt: st}
	c.entries[mask] = e
	c.mu.Unlock()
	c.hot.Store(e)
	return st
}

// Masks reports how many distinct dirty masks have compiled. `storm lint` uses
// it the same way it uses Shapes(): a writer that mints a statement per request
// shows up as a mask count that tracks traffic.
func (c *MaskCache) Masks() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// ErrStaleWrite is returned when an optimistic-locking UPDATE matched no row.
//
// It does not mean "nothing changed". It means the row's version is not the one
// that was read, so somebody else wrote it first and this update was computed
// from a value that is no longer true. Retry from a fresh read; do not force it.
var ErrStaleWrite = errors.New("storm: stale write — the row changed since it was read")

// ErrNoRow is returned when an update or delete addressed a row that is gone.
var ErrNoRow = errors.New("storm: no such row")

// InsertParts carries the punctuation of an INSERT from the back end to the
// splicer. Generated code fills it from compile/pgsql at build time; runtime
// never chooses any of it.
type InsertParts struct {
	Open, Sep, Mid, Close string
}

// SpliceInsert assembles an INSERT for one column set. Cold path: once per
// distinct set of assigned columns.
func SpliceInsert(prefix string, p InsertParts, cols []string, placeholder, suffix string) *Stmt {
	ph := Placeholder{}
	if placeholder != "" {
		ph.Sigil = placeholder[0]
	}
	return SpliceInsertWith(prefix, p, cols, ph, suffix)
}

// SpliceInsertWith is SpliceInsert with the placeholder CARRIER rather than a
// sigil string.
//
// The string form appends an ordinal unconditionally, which is PostgreSQL's
// `$1` with the sigil swapped — on a bare back end that is `?1`, and MySQL
// rejects it. The full-row insert escaped this because its SQL is a constant
// fixed at generate time; every PARTIAL insert and every queued one goes
// through here, so Create(), Ins and the whole unit of work were broken on
// MySQL and nothing had run one.
func SpliceInsertWith(prefix string, p InsertParts, cols []string, ph Placeholder, suffix string) *Stmt {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(p.Open)
	for i, c := range cols {
		if i > 0 {
			b.WriteString(p.Sep)
		}
		b.WriteString(c)
	}
	b.WriteString(p.Mid)
	for i := range cols {
		if i > 0 {
			b.WriteString(p.Sep)
		}
		ph.write(&b, i+1)
	}
	b.WriteString(p.Close)
	b.WriteString(suffix)
	return &Stmt{SQL: b.String(), NArg: len(cols)}
}

// ErrNothingAssigned is returned by an insert with no columns set. An INSERT
// naming no columns would take every default, which is almost never what the
// caller meant and is never what they said.
var ErrNothingAssigned = errors.New("storm: insert with no columns assigned")

// ErrConflict is returned by an insert that asked for DoNothing and found the
// row already there.
//
// It is its own error rather than ErrNoRow because the two mean opposite
// things about whether the caller has a problem: DO NOTHING suppresses the
// RETURNING row, so "no row" is the SUCCESS case of an idempotent insert, and
// a caller that treats it as a failure retries forever.
//
//	row, err := n.OnConflictEmail().DoNothing().Insert(ctx, ex)
//	switch {
//	case errors.Is(err, runtime.ErrConflict):
//	        // already there; row is zero
//	case err != nil:
//	        return err
//	}
var ErrConflict = errors.New("storm: the row already exists and DoNothing was asked for")

// ErrChildLimit is returned when a relation load reached its child limit.
//
// A partial relation load is worse than a failed one: every count computed from
// it is wrong and nothing says so. Raise ChildLimit or narrow the parent query.
var ErrChildLimit = errors.New("storm: relation load hit its child limit — the result would be silently partial")
