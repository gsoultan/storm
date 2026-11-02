package qcache_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/qcache"
)

// memStore is a Store in a map, with a switch to make every call fail.
type memStore struct {
	mu      sync.Mutex
	m       map[string][]byte
	ttl     map[string]time.Duration
	fail    error
	failSet error
	gets    int
}

func newStore() *memStore {
	return &memStore{m: map[string][]byte{}, ttl: map[string]time.Duration{}}
}

func (s *memStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.fail != nil {
		return nil, s.fail
	}
	return s.m[key], nil
}

func (s *memStore) MGet(_ context.Context, keys []string) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	out := make([][]byte, len(keys))
	for i, k := range keys {
		out[i] = s.m[k]
	}
	return out, nil
}

func (s *memStore) Set(_ context.Context, key string, v []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if s.failSet != nil {
		return s.failSet
	}
	s.m[key] = append([]byte(nil), v...)
	s.ttl[key] = ttl
	return nil
}

func (s *memStore) entries() (n int, ttls []time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, ttl := range s.ttl {
		if strings.Contains(k, ":q:") {
			n++
			ttls = append(ttls, ttl)
		}
	}
	return n, ttls
}

// fakeRows is a byte-shaped result.
type fakeRows struct {
	rows   [][][]byte
	at     int
	err    error // reported once the rows run out
	closed bool
}

func (r *fakeRows) Next() bool {
	if r.at >= len(r.rows) {
		return false
	}
	r.at++
	return true
}
func (r *fakeRows) RawValues() [][]byte { return r.rows[r.at-1] }
func (r *fakeRows) Close()              { r.closed = true }
func (r *fakeRows) Err() error {
	if r.at >= len(r.rows) {
		return r.err
	}
	return nil
}

// fakeValueRows is a value-shaped result.
type fakeValueRows struct {
	fakeRows
	vals [][]any
}

func (r *fakeValueRows) Next() bool {
	if r.at >= len(r.vals) {
		return false
	}
	r.at++
	return true
}
func (r *fakeValueRows) RawValues() [][]byte { return nil }
func (r *fakeValueRows) Values() []any       { return r.vals[r.at-1] }
func (r *fakeValueRows) Err() error          { return nil }

// fakeExec answers every query with a fresh copy of what result builds.
type fakeExec struct {
	queries, execs, copies, batches int
	result                          func() runtime.Rows
	queryErr                        error
}

func (e *fakeExec) Query(context.Context, string, []any) (runtime.Rows, error) {
	e.queries++
	if e.queryErr != nil {
		return nil, e.queryErr
	}
	return e.result(), nil
}
func (e *fakeExec) Exec(context.Context, string, []any) (int64, error) { e.execs++; return 3, nil }
func (e *fakeExec) CopyFrom(context.Context, string, []string, runtime.CopySource) (int64, error) {
	e.copies++
	return 4, nil
}
func (e *fakeExec) Batch(context.Context, []runtime.BatchOp, func(int, runtime.Rows, int64, error) error) error {
	e.batches++
	return nil
}

func byteResult() runtime.Rows {
	return &fakeRows{rows: [][][]byte{
		{[]byte("1"), []byte("ada"), nil},
		{[]byte("2"), []byte{}, []byte("x")},
	}}
}

func drain(t *testing.T, r runtime.Rows) [][][]byte {
	t.Helper()
	var out [][][]byte
	for r.Next() {
		var row [][]byte
		for _, c := range r.RawValues() {
			if c == nil {
				row = append(row, nil)
			} else {
				row = append(row, append([]byte{}, c...))
			}
		}
		out = append(out, row)
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	return out
}

func sameRows(t *testing.T, got, want [][][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("row %d: %d cells, want %d", i, len(got[i]), len(want[i]))
		}
		for j := range want[i] {
			if (got[i][j] == nil) != (want[i][j] == nil) || !bytes.Equal(got[i][j], want[i][j]) {
				t.Fatalf("row %d cell %d: %q (nil %v), want %q (nil %v)",
					i, j, got[i][j], got[i][j] == nil, want[i][j], want[i][j] == nil)
			}
		}
	}
}

func scoped() context.Context { return qcache.Scoped(context.Background(), "tenant-1") }

func TestUnmarkedQueryNeverTouchesTheStore(t *testing.T) {
	ex, st := &fakeExec{result: byteResult}, newStore()
	c := qcache.New(ex, st, qcache.Options{})
	for range 2 {
		drain(t, must(c.Query(scoped(), "q", nil)))
	}
	if ex.queries != 2 || st.gets != 0 {
		t.Fatalf("queries=%d gets=%d, want 2 and 0", ex.queries, st.gets)
	}
	// A non-positive TTL is how a caller turns caching off.
	drain(t, must(c.Query(qcache.For(scoped(), 0), "q", nil)))
	if st.gets != 0 {
		t.Fatal("a zero TTL consulted the store")
	}
}

func TestMissThenHitReplaysNullAndEmptyExactly(t *testing.T) {
	ex, st := &fakeExec{result: byteResult}, newStore()
	c := qcache.New(ex, st, qcache.Options{})
	ctx := qcache.For(scoped(), time.Minute)

	want := drain(t, byteResult())
	sameRows(t, drain(t, must(c.Query(ctx, "select", []any{int64(1)}))), want)
	got := must(c.Query(ctx, "select", []any{int64(1)}))
	if _, ok := got.(runtime.ValueRows); ok {
		t.Fatal("a byte-shaped hit satisfies ValueRows; a byte-shaped package would accept it as values")
	}
	sameRows(t, drain(t, got), want)
	if ex.queries != 1 {
		t.Fatalf("the database saw %d queries, want 1", ex.queries)
	}
	if s := c.Stats(); s.Hits != 1 || s.Misses != 1 || s.Stores != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestEmptyResultIsCached(t *testing.T) {
	ex := &fakeExec{result: func() runtime.Rows { return &fakeRows{} }}
	c := qcache.New(ex, newStore(), qcache.Options{})
	ctx := qcache.For(scoped(), time.Minute)
	for range 2 {
		if rows := drain(t, must(c.Query(ctx, "q", nil))); len(rows) != 0 {
			t.Fatalf("%d rows", len(rows))
		}
	}
	if ex.queries != 1 {
		t.Fatalf("queries=%d", ex.queries)
	}
}

func TestKeyIncludesScopeSQLAndArguments(t *testing.T) {
	ex, st := &fakeExec{result: byteResult}, newStore()
	c := qcache.New(ex, st, qcache.Options{})
	run := func(ctx context.Context, sql string, args ...any) {
		drain(t, must(c.Query(qcache.For(ctx, time.Minute), sql, args)))
	}
	bg := context.Background()
	run(qcache.Scoped(bg, "a"), "q", int64(1))
	run(qcache.Scoped(bg, "b"), "q", int64(1)) // another scope
	run(qcache.Scoped(bg, "a"), "r", int64(1)) // another statement
	run(qcache.Scoped(bg, "a"), "q", int64(2)) // another argument
	run(qcache.Scoped(bg, "a"), "q", int32(1)) // another argument type
	run(qcache.Scoped(bg, "a"), "q", int64(1)) // the first again: a hit
	if ex.queries != 5 {
		t.Fatalf("queries=%d, want 5", ex.queries)
	}
}

func TestNoScopeIsRefusedUnlessUnscoped(t *testing.T) {
	ex, st := &fakeExec{result: byteResult}, newStore()
	var reported []error
	c := qcache.New(ex, st, qcache.Options{OnError: func(err error) { reported = append(reported, err) }})
	ctx := qcache.For(context.Background(), time.Minute)
	for range 2 {
		drain(t, must(c.Query(ctx, "q", nil)))
	}
	if ex.queries != 2 || len(reported) != 2 || !errors.Is(reported[0], qcache.ErrNoScope) {
		t.Fatalf("queries=%d reported=%v", ex.queries, reported)
	}
	if s := c.Stats(); s.Bypassed != 2 || s.Errors != 2 {
		t.Fatalf("stats %+v", s)
	}

	ex2 := &fakeExec{result: byteResult}
	c2 := qcache.New(ex2, newStore(), qcache.Options{Unscoped: true})
	for range 2 {
		drain(t, must(c2.Query(ctx, "q", nil)))
	}
	if ex2.queries != 1 {
		t.Fatalf("Unscoped: queries=%d, want 1", ex2.queries)
	}
}

type opaque struct{ p *int }

func TestAnArgumentItCannotKeyOnRunsUncached(t *testing.T) {
	ex, st := &fakeExec{result: byteResult}, newStore()
	c := qcache.New(ex, st, qcache.Options{})
	ctx := qcache.For(scoped(), time.Minute)
	for range 2 {
		drain(t, must(c.Query(ctx, "q", []any{opaque{}})))
	}
	if ex.queries != 2 || st.gets != 0 || c.Stats().Bypassed != 2 {
		t.Fatalf("queries=%d gets=%d stats=%+v", ex.queries, st.gets, c.Stats())
	}
}

func TestAPrefixIsNeverStored(t *testing.T) {
	ex, st := &fakeExec{result: byteResult}, newStore()
	c := qcache.New(ex, st, qcache.Options{})
	ctx := qcache.For(scoped(), time.Minute)

	r := must(c.Query(ctx, "q", nil))
	r.Next() // a First: one row, then close
	r.Close()
	r.Close() // twice is harmless
	if n, _ := st.entries(); n != 0 {
		t.Fatal("a result read in part was stored")
	}

	ex.result = func() runtime.Rows { rr := byteResult().(*fakeRows); rr.err = errors.New("boom"); return rr }
	r = must(c.Query(ctx, "q", nil))
	for r.Next() {
	}
	if r.Err() == nil {
		t.Fatal("the inner error was not passed through")
	}
	r.Close()
	if n, _ := st.entries(); n != 0 {
		t.Fatal("a result that ended in an error was stored")
	}
}

func TestOversizedResultIsPassedThroughAndNotStored(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 100)
	ex := &fakeExec{result: func() runtime.Rows {
		return &fakeRows{rows: [][][]byte{{big}, {big}, {big}}}
	}}
	st := newStore()
	c := qcache.New(ex, st, qcache.Options{MaxBytes: 150})
	got := drain(t, must(c.Query(qcache.For(scoped(), time.Minute), "q", nil)))
	if len(got) != 3 || !bytes.Equal(got[2][0], big) {
		t.Fatal("the reader did not see the whole result")
	}
	if n, _ := st.entries(); n != 0 {
		t.Fatal("an oversized result was stored")
	}
}

func TestTTLIsCappedAtMaxTTL(t *testing.T) {
	st := newStore()
	c := qcache.New(&fakeExec{result: byteResult}, st, qcache.Options{MaxTTL: time.Minute})
	drain(t, must(c.Query(qcache.For(scoped(), 24*time.Hour), "q", nil)))
	if _, ttls := st.entries(); len(ttls) != 1 || ttls[0] != time.Minute {
		t.Fatalf("ttls %v, want [1m]", ttls)
	}
}

func TestInvalidateMakesTaggedEntriesUnreachable(t *testing.T) {
	ex, st := &fakeExec{result: byteResult}, newStore()
	c := qcache.New(ex, st, qcache.Options{})
	tagged := qcache.For(scoped(), time.Minute, "products")
	other := qcache.For(scoped(), time.Minute, "orders")
	read := func(ctx context.Context) { drain(t, must(c.Query(ctx, "q", nil))) }

	read(tagged)
	read(tagged)
	read(other)
	if ex.queries != 2 {
		t.Fatalf("queries=%d, want 2", ex.queries)
	}
	if err := c.Invalidate(context.Background(), "products"); err != nil {
		t.Fatal(err)
	}
	read(tagged) // miss: a new generation
	read(other)  // hit: not invalidated
	if ex.queries != 3 {
		t.Fatalf("queries=%d, want 3", ex.queries)
	}
	read(tagged)
	if ex.queries != 3 {
		t.Fatal("the entry stored after the invalidation was not hit")
	}
	for k, ttl := range st.ttl {
		if strings.Contains(k, ":t:products") && ttl != time.Hour+time.Minute {
			t.Fatalf("a generation lives %v; it must outlive MaxTTL", ttl)
		}
	}

	st.fail = errors.New("down")
	if err := c.Invalidate(context.Background(), "products"); err == nil {
		t.Fatal("a failed invalidation was swallowed")
	}
}

func TestAFailingStoreNeverFailsTheQuery(t *testing.T) {
	ex, st := &fakeExec{result: byteResult}, newStore()
	var reported int
	c := qcache.New(ex, st, qcache.Options{OnError: func(error) { reported++ }})
	st.fail = errors.New("connection refused")

	drain(t, must(c.Query(qcache.For(scoped(), time.Minute), "q", nil)))      // get and set fail
	drain(t, must(c.Query(qcache.For(scoped(), time.Minute, "t"), "q", nil))) // generations fail
	if ex.queries != 2 || reported != 3 {
		t.Fatalf("queries=%d reported=%d, want 2 and 3", ex.queries, reported)
	}

	st.fail, st.failSet = nil, errors.New("read only")
	drain(t, must(c.Query(qcache.For(scoped(), time.Minute), "q", nil)))
	if reported != 4 || c.Stats().Stores != 0 {
		t.Fatalf("reported=%d stores=%d", reported, c.Stats().Stores)
	}
}

// shortStore returns the wrong number of generations.
type shortStore struct{ *memStore }

func (shortStore) MGet(context.Context, []string) ([][]byte, error) { return nil, nil }

func TestAStoreThatMiscountsGenerationsIsBypassed(t *testing.T) {
	ex := &fakeExec{result: byteResult}
	c := qcache.New(ex, shortStore{newStore()}, qcache.Options{})
	for range 2 {
		drain(t, must(c.Query(qcache.For(scoped(), time.Minute, "t"), "q", nil)))
	}
	if ex.queries != 2 || c.Stats().Bypassed != 2 {
		t.Fatalf("queries=%d stats=%+v", ex.queries, c.Stats())
	}
}

func TestACorruptEntryIsAMiss(t *testing.T) {
	ex, st := &fakeExec{result: byteResult}, newStore()
	c := qcache.New(ex, st, qcache.Options{})
	ctx := qcache.For(scoped(), time.Minute)
	drain(t, must(c.Query(ctx, "q", nil)))

	want := drain(t, byteResult())
	for _, garbage := range [][]byte{
		{}, {9, 'b'}, {1, 'z'},
		{1, 'b', 5},          // more columns than bytes
		{1, 'b', 1},          // a column with no length
		{1, 'b', 1, 9, 'x'},  // a cell longer than what is left
		{1, 'v', 1},          // a value cell with no tag
		{1, 'v', 1, 99},      // an unknown tag
		{1, 'v', 9},          // more columns than bytes
		{1, 'v', 1, 1},       // an int64 with no varint
		{1, 'v', 1, 2, 0},    // a float64 short of 8 bytes
		{1, 'v', 1, 5, 9},    // bytes longer than what is left
		{1, 'v', 1, 7, 1, 0}, // a time that does not unmarshal
	} {
		for k := range st.m {
			if strings.Contains(k, ":q:") {
				st.m[k] = garbage
			}
		}
		sameRows(t, drain(t, must(c.Query(ctx, "q", nil))), want)
	}
}

func TestValueShapeRoundTrips(t *testing.T) {
	when := time.Date(2026, 11, 2, 6, 0, 0, 123456789, time.FixedZone("x", 3600))
	vals := [][]any{
		{int64(-7), 2.5, true, false, []byte("b"), "s", when, nil, []byte{}},
	}
	ex := &fakeExec{result: func() runtime.Rows { return &fakeValueRows{vals: vals} }}
	st := newStore()
	c := qcache.New(ex, st, qcache.Options{})
	ctx := qcache.For(scoped(), time.Minute)

	for i := range 2 {
		vr, err := runtime.AsValueRows(c.Query(ctx, "q", nil))
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if vr.RawValues() != nil {
			t.Fatal("RawValues before Next")
		}
		if !vr.Next() {
			t.Fatal("no row")
		}
		got := vr.Values()
		if vr.RawValues() != nil {
			t.Fatal("a value-shaped row has raw bytes")
		}
		if len(got) != len(vals[0]) {
			t.Fatalf("%d values", len(got))
		}
		for j, w := range vals[0] {
			switch w := w.(type) {
			case []byte:
				g, ok := got[j].([]byte)
				if w == nil {
					if got[j] != nil {
						t.Fatalf("value %d: %v, want nil", j, got[j])
					}
					continue
				}
				if !ok || !bytes.Equal(g, w) {
					t.Fatalf("value %d: %#v, want %#v", j, got[j], w)
				}
			case time.Time:
				if g, ok := got[j].(time.Time); !ok || !g.Equal(w) {
					t.Fatalf("value %d: %v, want %v", j, got[j], w)
				}
			default:
				if got[j] != w {
					t.Fatalf("value %d: %#v, want %#v", j, got[j], w)
				}
			}
		}
		if vr.Next() || vr.Err() != nil {
			t.Fatal("a second row")
		}
		if i == 1 && vr.Values() != nil {
			t.Fatal("a hit has values after its last row")
		}
		vr.Close()
	}
	if ex.queries != 1 {
		t.Fatalf("queries=%d, want 1", ex.queries)
	}
}

func TestAValueOutsideDriverValueIsNotStored(t *testing.T) {
	ex := &fakeExec{result: func() runtime.Rows { return &fakeValueRows{vals: [][]any{{int32(1)}}} }}
	st := newStore()
	c := qcache.New(ex, st, qcache.Options{})
	vr, err := runtime.AsValueRows(c.Query(qcache.For(scoped(), time.Minute), "q", nil))
	if err != nil {
		t.Fatal(err)
	}
	for vr.Next() {
		if vr.Values()[0] != int32(1) {
			t.Fatal("the reader did not see the value")
		}
	}
	vr.Close()
	if n, _ := st.entries(); n != 0 {
		t.Fatal("stored a value the codec cannot round-trip")
	}
}

func TestWritesPassThrough(t *testing.T) {
	ex := &fakeExec{result: byteResult}
	c := qcache.New(ex, newStore(), qcache.Options{})
	ctx := qcache.For(scoped(), time.Minute)
	if n, _ := c.Exec(ctx, "update", nil); n != 3 {
		t.Fatal("Exec")
	}
	if n, _ := c.CopyFrom(ctx, "t", nil, nil); n != 4 {
		t.Fatal("CopyFrom")
	}
	_ = c.Batch(ctx, nil, nil)
	if ex.execs != 1 || ex.copies != 1 || ex.batches != 1 {
		t.Fatalf("%+v", ex)
	}
	ex.queryErr = errors.New("syntax")
	if _, err := c.Query(ctx, "q", nil); err == nil {
		t.Fatal("a query error was swallowed")
	}
}

func must(r runtime.Rows, err error) runtime.Rows {
	if err != nil {
		panic(err)
	}
	return r
}

// An adopter who wraps their pool and marks nothing pays one context lookup
// per query, and no allocation.
func TestAnUnmarkedQueryAllocatesNothing(t *testing.T) {
	rows := &fakeRows{}
	ex := &fakeExec{result: func() runtime.Rows { return rows }}
	c := qcache.New(ex, newStore(), qcache.Options{})
	ctx := scoped()
	if n := testing.AllocsPerRun(100, func() { _, _ = c.Query(ctx, "q", nil) }); n != 0 {
		t.Fatalf("%v allocations per unmarked query, want 0", n)
	}
}
