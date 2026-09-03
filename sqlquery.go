package storm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gsoultan/storm/runtime"
)

// The typed escape hatch (M5).
//
// Anything PostgreSQL can run is expressible here — CTEs, windows, lateral
// joins — years before the native IR grows each construct. What storm adds is
// the part hand-rolled SQL always loses: the RESULT is typed, the scanner is
// generated, and the statement was validated against the model at GENERATE
// time, so a query whose columns drifted from its row type fails the build
// naming the column, not the 3am page.
//
//	var TopEarners = storm.SQL[EarnerRow](`
//	    WITH ranked AS (...)
//	    SELECT ... WHERE tenant_id = $1 ... LIMIT $2`)
//
//	rows, err := TopEarners.Query(ctx, db, tid, 3)   // []EarnerRow
//
// # How the scanner arrives
//
// `storm generate` PREPAREs the statement, matches the result descriptor
// against T's fields, and emits a scanner that registers itself by type in an
// init(). The first Query looks it up once and caches it in the value; the
// warm path is an atomic load. Running a query nothing generated for is an
// error naming the fix, not a reflective fallback — one reflection path
// becomes THE path.
//
// # Only a declared statement runs
//
// The same generate step emits a RegisterStatement for every statement it
// PREPAREd, and a declaration whose text is not among them is refused before
// the executor is reached. That is what keeps the escape hatch from being one:
// a scanner is keyed by ROW TYPE and would otherwise answer for a statement
// assembled at run time. See the statement-pinning note further down this
// file.
type SQLQuery[T any] struct {
	sql    string
	digest string
	nArg   int

	// ok caches the declared check. The lookup is a mutex and a map, so it
	// happens once per declaration and never on the warm path.
	ok   atomic.Bool
	scan atomic.Pointer[func([][]byte, *T, *runtime.Slab) error]
}

// SQL declares a raw query returning rows of T.
func SQL[T any](sql string) *SQLQuery[T] {
	return &SQLQuery[T]{sql: sql, digest: digestOf(sql), nArg: maxPlaceholder(sql)}
}

// Query runs the statement and scans every row.
//
// Args are variadic and checked against the statement's placeholder count
// before anything reaches the server; the ROW is where the typing lives, which
// is the half hand-rolled SQL cannot have.
func (q *SQLQuery[T]) Query(ctx context.Context, ex runtime.Executor, args ...any) ([]T, error) {
	if len(args) != q.nArg {
		return nil, fmt.Errorf("storm: query wants %d argument(s), got %d", q.nArg, len(args))
	}
	if !declared(q.digest, &q.ok) {
		return nil, undeclared(q.sql)
	}
	scan := q.scanner()
	if scan == nil {
		var zero T
		return nil, fmt.Errorf(
			"storm: no scanner generated for %T — run 'storm generate' with this query registered",
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
	sql    string
	digest string
	nArg   int

	ok atomic.Bool
}

// SQLExec declares a raw statement executed for its effect.
func SQLExec(sql string) *SQLStmt {
	return &SQLStmt{sql: sql, digest: digestOf(sql), nArg: maxPlaceholder(sql)}
}

// Exec runs the statement and reports rows affected.
func (q *SQLStmt) Exec(ctx context.Context, ex runtime.Executor, args ...any) (int64, error) {
	if len(args) != q.nArg {
		return 0, fmt.Errorf("storm: statement wants %d argument(s), got %d", q.nArg, len(args))
	}
	if !declared(q.digest, &q.ok) {
		return 0, undeclared(q.sql)
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

// Statement pinning: only a statement the generator saw will run.
//
// Everywhere else in storm a caller's value cannot reach SQL text — a
// predicate is a stream of compiler-generated ids and the values travel as
// bound arguments, so there is no string to escape and nothing to get wrong.
// The escape hatch is the exception, and one detail makes it sharper than it
// looks: RegisterScanner keys by ROW TYPE, so a scanner declared for one query
// answers for ANY query returning that type. Without this check
// `storm.SQL[Row](fmt.Sprintf(..., userInput))` would execute on the strength
// of a scanner it never declared.
//
// So `storm generate` emits every statement it PREPAREd, and a declaration
// that is not among them does not run. A statement assembled at run time now
// fails at the call, naming the fix, instead of reaching the server.
var (
	stmtMu   sync.Mutex
	declStmt = map[string]struct{}{}
)

// RegisterStatement records a statement that `storm generate` PREPAREd and
// validated against the model. Generated code calls it from an init().
//
// It takes the statement TEXT rather than a digest so the generated init is
// reviewable: what a reader needs to check is which statements are allowed to
// run, and a list of hex is not that.
func RegisterStatement(sql string) {
	d := digestOf(sql)
	stmtMu.Lock()
	declStmt[d] = struct{}{}
	stmtMu.Unlock()
}

// digestOf identifies a statement by content. SHA-256 rather than the text
// itself as the map key: a declaration is checked once and then cached, and
// the set stays a fixed size per statement however long the SQL is.
func digestOf(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

// declared reports whether digest was registered, caching a hit in ok so the
// mutex and the map are touched once per declaration and never per call.
//
// A miss is NOT cached: every registration happens in an init(), so a miss
// after that is permanent, and re-checking costs a mutex on a path that is
// about to return an error anyway.
func declared(digest string, ok *atomic.Bool) bool {
	if ok.Load() {
		return true
	}
	stmtMu.Lock()
	_, hit := declStmt[digest]
	stmtMu.Unlock()
	if hit {
		ok.Store(true)
	}
	return hit
}

// undeclared explains the refusal. It quotes the statement back because the
// call site is usually a variable name, and the first question a reader has is
// which statement storm means.
func undeclared(sql string) error {
	return fmt.Errorf(
		"storm: this statement was not declared at generate time, so it will not run:\n"+
			"  %s\n"+
			"Either it was added since the last 'storm generate', or it was assembled at run "+
			"time — which is the one way a caller's string can reach SQL text, since every "+
			"other query storm issues carries none. Declare it as a package-level var and "+
			"pass values as $1 arguments:\n"+
			"  var Q = storm.SQL[Row](`SELECT ... WHERE tenant = $1`)\n"+
			"  rows, err := Q.Query(ctx, db, tenantID)\n"+
			"then run 'storm generate'.",
		firstLine(sql))
}

// firstLine trims a statement to something an error can carry.
func firstLine(sql string) string {
	s := strings.TrimSpace(sql)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " ..."
	}
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
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
