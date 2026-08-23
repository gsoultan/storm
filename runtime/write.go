package runtime

import (
	"errors"
	"strconv"
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
			b.WriteString(f.A)
			if takesArg(f) {
				ord++
				b.WriteString(strconv.Itoa(ord))
			}
			b.WriteString(f.B)
		}
	}
	b.WriteString(suffix)
	return &Stmt{SQL: b.String(), NArg: ord}
}

// MaskCache maps a dirty mask to a compiled statement.
type MaskCache struct {
	last atomic.Uint64
	hot  atomic.Pointer[Stmt]

	mu      sync.RWMutex
	entries map[uint64]*Stmt
}

func NewMaskCache() *MaskCache { return &MaskCache{entries: map[uint64]*Stmt{}} }

// Get returns the statement for a mask, or nil. Allocation-free, and on the
// common case of one mask repeated it is two atomic loads and a compare.
func (c *MaskCache) Get(mask uint64) *Stmt {
	if c.last.Load() == mask {
		if st := c.hot.Load(); st != nil {
			return st
		}
	}
	c.mu.RLock()
	st := c.entries[mask]
	c.mu.RUnlock()
	if st != nil {
		c.hot.Store(st)
		c.last.Store(mask)
	}
	return st
}

// Put interns a statement. Two goroutines compiling the same mask is harmless;
// the first one interned wins and both return the same pointer, so a shape
// never has two slab hints racing.
func (c *MaskCache) Put(mask uint64, st *Stmt) *Stmt {
	c.mu.Lock()
	if prev, ok := c.entries[mask]; ok {
		c.mu.Unlock()
		return prev
	}
	c.entries[mask] = st
	c.mu.Unlock()
	c.hot.Store(st)
	c.last.Store(mask)
	return st
}

// Masks reports how many distinct dirty masks have compiled. `raorm lint` uses
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
var ErrStaleWrite = errors.New("raorm: stale write — the row changed since it was read")

// ErrNoRow is returned when an update or delete addressed a row that is gone.
var ErrNoRow = errors.New("raorm: no such row")

// InsertParts carries the punctuation of an INSERT from the back end to the
// splicer. Generated code fills it from compile/pgsql at build time; runtime
// never chooses any of it.
type InsertParts struct {
	Open, Sep, Mid, Close string
}

// SpliceInsert assembles an INSERT for one column set. Cold path: once per
// distinct set of assigned columns.
func SpliceInsert(prefix string, p InsertParts, cols []string, placeholder, suffix string) *Stmt {
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
		b.WriteString(placeholder)
		b.WriteString(strconv.Itoa(i + 1))
	}
	b.WriteString(p.Close)
	b.WriteString(suffix)
	return &Stmt{SQL: b.String(), NArg: len(cols)}
}

// ErrNothingAssigned is returned by an insert with no columns set. An INSERT
// naming no columns would take every default, which is almost never what the
// caller meant and is never what they said.
var ErrNothingAssigned = errors.New("raorm: insert with no columns assigned")

// ErrChildLimit is returned when a relation load reached its child limit.
//
// A partial relation load is worse than a failed one: every count computed from
// it is wrong and nothing says so. Raise ChildLimit or narrow the parent query.
var ErrChildLimit = errors.New("raorm: relation load hit its child limit — the result would be silently partial")
