package spike

import (
	"context"
	"sync"
	"time"
)

// Query is a value type. Every builder method returns a copy, so composing a
// query allocates nothing: the predicates live in fixed fields and the shape
// lives in a uint32.
type Query struct {
	mask   uint32
	org    [16]byte
	email  string
	name   string
	age    int32
	status string
	since  time.Time
	limit  int32
}

func New() Query { return Query{limit: 50} }

func (q Query) Org(v [16]byte) Query    { q.mask |= fOrg; q.org = v; return q }
func (q Query) Email(v string) Query    { q.mask |= fEmail; q.email = v; return q }
func (q Query) NameLike(v string) Query { q.mask |= fName; q.name = v; return q }
func (q Query) AgeGte(v int32) Query    { q.mask |= fAge; q.age = v; return q }
func (q Query) Status(v string) Query   { q.mask |= fStatus; q.status = v; return q }
func (q Query) Since(v time.Time) Query { q.mask |= fSince; q.since = v; return q }
func (q Query) Limit(v int32) Query     { q.limit = v; return q }

// Shape is the compiled-statement key. Exposed for benchmarks and lint.
func (q Query) Shape() uint32 { return q.mask }

// SQL returns the compiled text for this shape, compiling on first use.
func (q Query) SQL() string { return lookup(q.mask).sql }

// Binder holds both the argument values and the []any handed to pgx. The
// interface entries point at this struct's own fields, so boxing a pointer
// never allocates, and the struct itself is pooled.
type Binder struct {
	vals   []any
	org    [16]byte
	email  string
	name   string
	age    int32
	status string
	since  time.Time
	limit  int32
}

var binders = sync.Pool{
	New: func() any { return &Binder{vals: make([]any, 0, nFilters+1)} },
}

// GetBinder / PutBinder are the pooled-argument lifecycle. Callers own the
// Binder for the duration of a query. Returning a closure instead would cost
// one allocation per call — exactly the allocation this design exists to
// remove.
func GetBinder() *Binder  { return binders.Get().(*Binder) }
func PutBinder(b *Binder) { binders.Put(b) }

// bind fills the pooled binder and returns the argument slice. Zero allocations
// on a warm path: the backing array is reused and every element is a pointer.
func (q Query) bind(b *Binder) []any {
	v := b.vals[:0]
	if q.mask&fOrg != 0 {
		b.org = q.org
		v = append(v, &b.org)
	}
	if q.mask&fEmail != 0 {
		b.email = q.email
		v = append(v, &b.email)
	}
	if q.mask&fName != 0 {
		b.name = q.name
		v = append(v, &b.name)
	}
	if q.mask&fAge != 0 {
		b.age = q.age
		v = append(v, &b.age)
	}
	if q.mask&fStatus != 0 {
		b.status = q.status
		v = append(v, &b.status)
	}
	if q.mask&fSince != 0 {
		b.since = q.since
		v = append(v, &b.since)
	}
	b.limit = q.limit
	v = append(v, &b.limit)
	b.vals = v
	return v
}

// Prepare does everything except talk to the database: resolve the shape,
// compile if cold, and bind arguments. This is the path the thesis claims is
// allocation-free once warm.
func (q Query) Prepare(b *Binder) (sql string, args []any) {
	return lookup(q.mask).sql, q.bind(b)
}

// Rows is the slice of the executor surface the spike needs.
type Rows interface {
	Next() bool
	RawValues() [][]byte
	Close()
	Err() error
}

// Executor is the driver port. pgx never crosses it.
type Executor interface {
	Query(ctx context.Context, sql string, args []any) (Rows, error)
}

// All runs the query and decodes every row with the generated-style scanner.
func (q Query) All(ctx context.Context, ex Executor, dst []Row) ([]Row, error) {
	var sl Slab
	return q.AllInto(ctx, ex, dst, &sl)
}

// AllInto lets the caller own the string arena, so a hot loop can amortise it
// across queries when it knows the previous result is dead.
func (q Query) AllInto(ctx context.Context, ex Executor, dst []Row, sl *Slab) ([]Row, error) {
	b := GetBinder()
	defer PutBinder(b)
	st := lookup(q.mask)
	sl.Reserve(int(st.hint.Load()))

	rows, err := ex.Query(ctx, st.sql, q.bind(b))
	if err != nil {
		return dst, err
	}
	defer rows.Close()

	for rows.Next() {
		dst = append(dst, Row{})
		scanRow(rows.RawValues(), &dst[len(dst)-1], sl)
	}
	st.hint.Store(int64(sl.Size()))
	return dst, rows.Err()
}

// getSQL is the statement a generator emits for a primary-key lookup: no
// shape, no filters, one placeholder.
const getSQL = selectPrefix + ` WHERE id = $1`

type getBinder struct {
	vals []any
	id   [16]byte
}

var getBinders = sync.Pool{
	New: func() any { return &getBinder{vals: make([]any, 1)} },
}

// Get is the one-liner path: fetch by primary key.
func Get(ctx context.Context, ex Executor, id [16]byte, dst *Row, sl *Slab) (bool, error) {
	b := getBinders.Get().(*getBinder)
	defer getBinders.Put(b)
	b.id = id
	b.vals[0] = &b.id

	rows, err := ex.Query(ctx, getSQL, b.vals)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	if !rows.Next() {
		return false, rows.Err()
	}
	scanRow(rows.RawValues(), dst, sl)
	return true, rows.Err()
}

// SQLForShape returns the compiled text for an arbitrary shape mask. Test-only:
// the generated API never lets a caller name a shape directly.
func (Query) SQLForShape(mask uint32) string { return lookup(mask).sql }
