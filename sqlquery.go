package raorm

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/gsoultan/raorm/runtime"
)

// The typed escape hatch (M5).
//
// Anything PostgreSQL can run is expressible here — CTEs, windows, lateral
// joins — years before the native IR grows each construct. What raorm adds is
// the part hand-rolled SQL always loses: the RESULT is typed, the scanner is
// generated, and the statement was validated against the model at GENERATE
// time, so a query whose columns drifted from its row type fails the build
// naming the column, not the 3am page.
//
//	var TopEarners = raorm.SQL[EarnerRow](`
//	    WITH ranked AS (...)
//	    SELECT ... WHERE tenant_id = $1 ... LIMIT $2`)
//
//	rows, err := TopEarners.Query(ctx, db, tid, 3)   // []EarnerRow
//
// # How the scanner arrives
//
// `raorm generate` PREPAREs the statement, matches the result descriptor
// against T's fields, and emits a scanner that registers itself by type in an
// init(). The first Query looks it up once and caches it in the value; the
// warm path is an atomic load. Running a query nothing generated for is an
// error naming the fix, not a reflective fallback — one reflection path
// becomes THE path.
type SQLQuery[T any] struct {
	sql  string
	nArg int

	scan atomic.Pointer[func([][]byte, *T, *runtime.Slab) error]
}

// SQL declares a raw query returning rows of T.
func SQL[T any](sql string) *SQLQuery[T] {
	return &SQLQuery[T]{sql: sql, nArg: maxPlaceholder(sql)}
}

// Query runs the statement and scans every row.
//
// Args are variadic and checked against the statement's placeholder count
// before anything reaches the server; the ROW is where the typing lives, which
// is the half hand-rolled SQL cannot have.
func (q *SQLQuery[T]) Query(ctx context.Context, ex runtime.Executor, args ...any) ([]T, error) {
	if len(args) != q.nArg {
		return nil, fmt.Errorf("raorm: query wants %d argument(s), got %d", q.nArg, len(args))
	}
	scan := q.scanner()
	if scan == nil {
		var zero T
		return nil, fmt.Errorf(
			"raorm: no scanner generated for %T — run 'raorm generate' with this query registered",
			zero)
	}

	rows, err := ex.Query(ctx, q.sql, args)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sl runtime.Slab
	var out []T
	for rows.Next() {
		out = append(out, *new(T))
		if err := scan(rows.RawValues(), &out[len(out)-1], &sl); err != nil {
			return nil, err
		}
	}
	return out, rows.Err()
}

// One runs the statement and returns the first row, if any.
func (q *SQLQuery[T]) One(ctx context.Context, ex runtime.Executor, args ...any) (T, bool, error) {
	var zero T
	rows, err := q.Query(ctx, ex, args...)
	if err != nil || len(rows) == 0 {
		return zero, false, err
	}
	return rows[0], true, nil
}

// decl is what the generator reads off a registered query.
func (q *SQLQuery[T]) decl() (reflect.Type, string) {
	var zero T
	return reflect.TypeOf(zero), q.sql
}

// SQLStmt is the no-rows half of the escape hatch: DELETEs, junction-table
// INSERTs, `SELECT maintenance_fn(...)` calls. It carries no row type — and
// the generator enforces that, failing generation if the statement's result
// descriptor has columns, so "I meant to read those rows" cannot compile
// into silently dropping them.
type SQLStmt struct {
	sql  string
	nArg int
}

// SQLExec declares a raw statement executed for its effect.
func SQLExec(sql string) *SQLStmt {
	return &SQLStmt{sql: sql, nArg: maxPlaceholder(sql)}
}

// Exec runs the statement and reports rows affected.
func (q *SQLStmt) Exec(ctx context.Context, ex runtime.Executor, args ...any) (int64, error) {
	if len(args) != q.nArg {
		return 0, fmt.Errorf("raorm: statement wants %d argument(s), got %d", q.nArg, len(args))
	}
	return ex.Exec(ctx, q.sql, args)
}

// decl reports a nil row type: the generator PREPAREs and validates the
// statement like any other declaration, but resolves no scanner for it.
func (q *SQLStmt) decl() (reflect.Type, string) { return nil, q.sql }

// RawDecl is implemented by every SQLQuery, so a bootstrap can register them
// as a plain []any the way it registers models.
type RawDecl interface {
	decl() (reflect.Type, string)
}

// DeclOf reads a registered query's row type and SQL; the generate command
// uses it and nothing else should.
func DeclOf(d RawDecl) (reflect.Type, string) { rt, s := d.decl(); return rt, s }

func (q *SQLQuery[T]) scanner() func([][]byte, *T, *runtime.Slab) error {
	if p := q.scan.Load(); p != nil {
		return *p
	}
	// First call: one registry lookup, then cached. The warm path above is an
	// atomic load and nothing else.
	var zero T
	scanMu.Lock()
	fn, ok := scanners[reflect.TypeOf(zero)]
	scanMu.Unlock()
	if !ok {
		return nil
	}
	typed := fn.(func([][]byte, *T, *runtime.Slab) error)
	q.scan.Store(&typed)
	return typed
}

var (
	scanMu   sync.Mutex
	scanners = map[reflect.Type]any{}
)

// RegisterScanner is called by generated code from an init(). One scanner per
// row type: two queries sharing a row type share its scanner, which is safe
// because the generator validated both against the same descriptor shape.
func RegisterScanner[T any](fn func([][]byte, *T, *runtime.Slab) error) {
	var zero T
	scanMu.Lock()
	scanners[reflect.TypeOf(zero)] = fn
	scanMu.Unlock()
}

// maxPlaceholder finds the highest $n in the statement — the argument count a
// call must supply. Dollar-quoted strings and casts do not produce false
// positives worth defending against here: a wrong count fails loudly on the
// first call, at the caller, with both numbers in the message.
func maxPlaceholder(sql string) int {
	max := 0
	for i := 0; i+1 < len(sql); i++ {
		if sql[i] != '$' {
			continue
		}
		n, j := 0, i+1
		for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
			n = n*10 + int(sql[j]-'0')
			j++
		}
		if j > i+1 && n > max {
			max = n
		}
	}
	return max
}
