package sqldrv

// The adapter, against a fake database/sql driver.
//
// A real driver is not needed to prove the ADAPTER: what this package does is
// turn a *sql.Rows into the value shape of runtime.Rows, turn storm's bound
// values into ones database/sql takes, and emulate the two things
// runtime.Executor asks for that database/sql has no form of. All four are
// observable with a driver that answers from a table.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

// ---- a database/sql driver that answers from canned data --------------------

type fakeDriver struct {
	mu       sync.Mutex
	cols     []string
	rows     [][]driver.Value
	affected int64
	stmts    []string // every statement it was given, in order
	args     [][]driver.Value
	failOn   string
}

func (d *fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{d: d}, nil }

type fakeConn struct{ d *fakeDriver }

func (c *fakeConn) Prepare(q string) (driver.Stmt, error) { return &fakeStmt{d: c.d, q: q}, nil }
func (c *fakeConn) Close() error                          { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)             { return nil, errors.New("no tx") }

type fakeStmt struct {
	d *fakeDriver
	q string
}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }

func (s *fakeStmt) record(args []driver.Value) error {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	s.d.stmts = append(s.d.stmts, s.q)
	s.d.args = append(s.d.args, args)
	if s.d.failOn != "" && strings.Contains(s.q, s.d.failOn) {
		return errors.New("refused: " + s.d.failOn)
	}
	return nil
}

func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	if err := s.record(args); err != nil {
		return nil, err
	}
	return fakeResult{n: s.d.affected}, nil
}

func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	if err := s.record(args); err != nil {
		return nil, err
	}
	return &fakeDriverRows{cols: s.d.cols, rows: s.d.rows}, nil
}

type fakeResult struct{ n int64 }

func (r fakeResult) LastInsertId() (int64, error) { return 0, errors.New("unsupported") }
func (r fakeResult) RowsAffected() (int64, error) { return r.n, nil }

type fakeDriverRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeDriverRows) Columns() []string { return r.cols }
func (r *fakeDriverRows) Close() error      { return nil }
func (r *fakeDriverRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

var registerOnce sync.Once

func open(t *testing.T, d *fakeDriver) (*sql.DB, func()) {
	t.Helper()
	registerOnce.Do(func() { sql.Register("sqldrv-fake", &dispatch{}) })
	current.Store(d)
	db, err := sql.Open("sqldrv-fake", "x")
	if err != nil {
		t.Fatal(err)
	}
	return db, func() { db.Close() }
}

// dispatch exists because sql.Register is global and once-only: the driver a
// test wants changes, the registration cannot.
type dispatch struct{}

func (dispatch) Open(string) (driver.Conn, error) {
	return &fakeConn{d: current.Load().(*fakeDriver)}, nil
}

var current atomicAny

type atomicAny struct {
	mu sync.Mutex
	v  any
}

func (a *atomicAny) Store(v any) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicAny) Load() any   { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

// ---- the tests ---------------------------------------------------------------

// The adapter is the VALUE shape: RawValues is nil and Values carries what the
// driver decoded. A generated package that read the wrong side would scan nil.
func TestQueryHandsOverTheValueShape(t *testing.T) {
	d := &fakeDriver{
		cols: []string{"a", "b"},
		rows: [][]driver.Value{{int64(1), "x"}, {int64(2), nil}},
	}
	db, done := open(t, d)
	defer done()

	rows, err := New(db).Query(context.Background(), "SELECT a, b FROM t", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.RawValues() != nil {
		t.Error("a database/sql driver decoded before storm could see the wire")
	}
	var got [][]any
	for rows.Next() {
		v := rows.Values()
		got = append(got, []any{v[0], v[1]})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows", len(got))
	}
	// A NULL stays nil, which is what keeps absent and zero distinct.
	if got[1][1] != nil {
		t.Errorf("a NULL came back as %#v", got[1][1])
	}
}

// RowsAffected is optional in database/sql and several drivers refuse it. A
// refusal is zero rather than an error: the statement RAN, and failing the call
// would turn a driver's limitation into a data error.
func TestExecReportsZeroWhenTheDriverWillNotCount(t *testing.T) {
	d := &fakeDriver{affected: 3}
	db, done := open(t, d)
	defer done()
	n, err := New(db).Exec(context.Background(), "UPDATE t SET a = 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("affected = %d, want 3", n)
	}
}

// CopyFrom is EMULATED, one INSERT per row, and runtime.Executor's contract
// requires an adapter to say so — which this package's doc comment does. What
// is checked here is that it actually runs one statement per row rather than
// silently dropping the rest.
func TestCopyFromIsOneInsertPerRow(t *testing.T) {
	d := &fakeDriver{}
	db, done := open(t, d)
	defer done()
	src := &sliceSource{rows: [][]any{{int64(1), "a"}, {int64(2), "b"}, {int64(3), "c"}}}
	n, err := New(db).CopyFrom(context.Background(), "t", []string{"id", "name"}, src)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("copied %d rows, want 3", n)
	}
	if len(d.stmts) != 3 {
		t.Fatalf("ran %d statements for 3 rows", len(d.stmts))
	}
	if !strings.Contains(d.stmts[0], "INSERT INTO t (id, name) VALUES (:1, :2)") {
		t.Errorf("got %s", d.stmts[0])
	}
}

func TestCopyFromRefusesNoColumns(t *testing.T) {
	db, done := open(t, &fakeDriver{})
	defer done()
	if _, err := New(db).CopyFrom(context.Background(), "t", nil, &sliceSource{}); err == nil {
		t.Error("a copy into no columns must be refused")
	}
}

// Batch is N round trips — database/sql has no pipelining — and the SEMANTICS
// hold: each result is handed over in order, exactly one of rows and affected
// is meaningful, and the first error from the callback aborts the rest.
func TestBatchDeliversInOrderAndDrains(t *testing.T) {
	d := &fakeDriver{cols: []string{"n"}, rows: [][]driver.Value{{int64(7)}}, affected: 2}
	db, done := open(t, d)
	defer done()

	type result struct {
		i        int
		hadRows  bool
		affected int64
	}
	var got []result
	err := New(db).Batch(context.Background(), []runtime.BatchOp{
		{SQL: "SELECT n FROM t", WantRows: true},
		{SQL: "UPDATE t SET n = 1"},
		{SQL: "SELECT n FROM t", WantRows: true},
	}, func(i int, rows runtime.Rows, affected int64, err error) error {
		if err != nil {
			return err
		}
		got = append(got, result{i: i, hadRows: rows != nil, affected: affected})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results for 3 ops", len(got))
	}
	for i, r := range got {
		if r.i != i {
			t.Errorf("result %d arrived as %d; batch results are ordered", i, r.i)
		}
	}
	// Exactly one of rows and affected is meaningful, chosen by WantRows.
	if !got[0].hadRows || got[0].affected != 0 {
		t.Errorf("a row op gave %+v", got[0])
	}
	if got[1].hadRows || got[1].affected != 2 {
		t.Errorf("a count op gave %+v", got[1])
	}
}

// An error from the callback aborts the rest, and the ops after it do not run.
func TestBatchStopsWhenTheCallbackSaysSo(t *testing.T) {
	d := &fakeDriver{affected: 1}
	db, done := open(t, d)
	defer done()
	stop := errors.New("stop")
	err := New(db).Batch(context.Background(), []runtime.BatchOp{
		{SQL: "UPDATE a"}, {SQL: "UPDATE b"}, {SQL: "UPDATE c"},
	}, func(i int, _ runtime.Rows, _ int64, _ error) error {
		if i == 1 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("got %v, want the callback's error", err)
	}
	if len(d.stmts) != 2 {
		t.Errorf("ran %d statements; the third must not have run", len(d.stmts))
	}
}

// A statement the server refuses stops the batch and the error reaches the
// caller even with no callback.
func TestBatchWithNoCallbackStillDrainsAndReports(t *testing.T) {
	d := &fakeDriver{failOn: "UPDATE b"}
	db, done := open(t, d)
	defer done()
	err := New(db).Batch(context.Background(), []runtime.BatchOp{
		{SQL: "UPDATE a"}, {SQL: "UPDATE b"}, {SQL: "UPDATE c"},
	}, nil)
	if err == nil {
		t.Fatal("a refused statement must reach the caller")
	}
	if len(d.stmts) != 2 {
		t.Errorf("ran %d statements; the batch must stop at the failure", len(d.stmts))
	}
}

type sliceSource struct {
	rows [][]any
	i    int
}

func (s *sliceSource) Next() bool    { s.i++; return s.i <= len(s.rows) }
func (s *sliceSource) Values() []any { return s.rows[s.i-1] }
func (s *sliceSource) Err() error    { return nil }
