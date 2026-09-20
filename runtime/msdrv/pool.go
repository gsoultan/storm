package msdrv

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// Defaults, matching runtime/mydrv's so a caller configuring two adapters gets
// the same behaviour from both.
const (
	DefaultMaxConns       = 10
	DefaultAcquireTimeout = 10 * time.Second
)

// ErrPoolClosed is returned by a pool that has been closed.
var ErrPoolClosed = errors.New("msdrv: the pool is closed")

// ErrPoolExhausted is returned when every connection is out and the acquire
// timeout expires.
//
// An error rather than a hang, for the reason runtime/mydrv's note gives: a
// result set holds its connection until Close, so one caller who forgets to
// close takes a connection out of the pool permanently, and a service that
// stops answering is worse than one that says why.
var ErrPoolExhausted = errors.New(
	"msdrv: no connection became available before the acquire timeout — every connection is " +
		"in use, which usually means a result set somewhere was not closed")

// Pool is a set of connections, and the Executor storm is given.
type Pool struct {
	cfg  Config
	max  int
	wait time.Duration

	mu     sync.Mutex
	idle   []*Conn
	live   int
	closed bool
	// free is signalled when a connection returns.
	free chan struct{}
}

// NewPool opens one connection to prove the configuration works, and keeps it.
//
// Eagerly, because a pool that only fails on first use moves a configuration
// error out of startup and into the first request — where it is a 500 rather
// than a process that refuses to start.
func NewPool(ctx context.Context, cfg Config) (*Pool, error) {
	p := &Pool{cfg: cfg, max: cfg.MaxConns, wait: cfg.AcquireTimeout, free: make(chan struct{}, 1)}
	if p.max <= 0 {
		p.max = DefaultMaxConns
	}
	if p.wait == 0 {
		p.wait = DefaultAcquireTimeout
	}
	c, err := Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	p.idle = append(p.idle, c)
	p.live = 1
	return p, nil
}

func (p *Pool) acquire(ctx context.Context) (*Conn, error) {
	deadline := time.Now().Add(p.wait)
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrPoolClosed
		}
		if n := len(p.idle); n > 0 {
			c := p.idle[n-1]
			p.idle = p.idle[:n-1]
			p.mu.Unlock()
			return c, nil
		}
		if p.live < p.max {
			p.live++
			p.mu.Unlock()
			c, err := Open(ctx, p.cfg)
			if err != nil {
				p.mu.Lock()
				p.live--
				p.mu.Unlock()
				return nil, err
			}
			return c, nil
		}
		p.mu.Unlock()

		var timeout <-chan time.Time
		if p.wait > 0 {
			d := time.Until(deadline)
			if d <= 0 {
				return nil, ErrPoolExhausted
			}
			t := time.NewTimer(d)
			defer t.Stop()
			timeout = t.C
		}
		select {
		case <-p.free:
		case <-timeout:
			return nil, ErrPoolExhausted
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// release returns a connection, discarding one whose stream is no longer
// trustworthy.
func (p *Pool) release(c *Conn) {
	p.mu.Lock()
	if p.closed || c.bad || c.busy {
		p.live--
		p.mu.Unlock()
		c.Close()
		p.signal()
		return
	}
	p.idle = append(p.idle, c)
	p.mu.Unlock()
	p.signal()
}

func (p *Pool) signal() {
	select {
	case p.free <- struct{}{}:
	default:
	}
}

// Close closes every idle connection. One in use is closed when it returns.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, c := range idle {
		c.Close()
	}
}

// Query runs a statement that returns rows.
//
// The connection is held until the rows are CLOSED, which is why Rows.Close is
// not optional: the result is streamed off that connection and nothing else can
// use it until the stream is done.
func (p *Pool) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.Query(ctx, sql, args)
	if err != nil {
		p.release(c)
		return nil, err
	}
	return &pooledRows{Rows: rows.(*Rows), p: p, c: c}, nil
}

func (p *Pool) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer p.release(c)
	return c.Exec(ctx, sql, args)
}

func (p *Pool) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer p.release(c)
	return c.CopyFrom(ctx, table, cols, src)
}

func (p *Pool) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(int, runtime.Rows, int64, error) error) error {
	c, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer p.release(c)
	return c.Batch(ctx, ops, each)
}

// pooledRows returns the connection when the result is closed.
type pooledRows struct {
	*Rows
	p    *Pool
	c    *Conn
	once sync.Once
}

func (r *pooledRows) Close() {
	r.Rows.Close()
	r.once.Do(func() { r.p.release(r.c) })
}

// Begin starts a transaction, which is an Executor rather than a set of methods
// on one — see the note at the bottom of runtime/exec.go.
func (p *Pool) Begin(ctx context.Context) (*Tx, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := c.Exec(ctx, "BEGIN TRANSACTION", nil); err != nil {
		p.release(c)
		return nil, err
	}
	return &Tx{p: p, c: c}, nil
}

// Tx is a transaction: an Executor whose statements share one connection.
type Tx struct {
	p    *Pool
	c    *Conn
	done bool
}

// ErrTxDone is returned by a transaction that has already committed or rolled
// back.
var ErrTxDone = errors.New("msdrv: the transaction has already finished")

func (t *Tx) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	if t.done {
		return nil, ErrTxDone
	}
	return t.c.Query(ctx, sql, args)
}

func (t *Tx) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	if t.done {
		return 0, ErrTxDone
	}
	return t.c.Exec(ctx, sql, args)
}

func (t *Tx) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	if t.done {
		return 0, ErrTxDone
	}
	return t.c.CopyFrom(ctx, table, cols, src)
}

func (t *Tx) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(int, runtime.Rows, int64, error) error) error {
	if t.done {
		return ErrTxDone
	}
	return t.c.Batch(ctx, ops, each)
}

func (t *Tx) Commit(ctx context.Context) error { return t.end(ctx, "COMMIT TRANSACTION") }

// Rollback is safe to call on a finished transaction, so `defer tx.Rollback`
// beside a commit is the idiom rather than a bug.
func (t *Tx) Rollback(ctx context.Context) error {
	if t.done {
		return nil
	}
	return t.end(ctx, "ROLLBACK TRANSACTION")
}

func (t *Tx) end(ctx context.Context, verb string) error {
	if t.done {
		return ErrTxDone
	}
	t.done = true
	_, err := t.c.Exec(ctx, verb, nil)
	t.p.release(t.c)
	return err
}

// rowsPerInsert is how many rows one emulated COPY statement carries.
//
// SQL Server caps a VALUES clause at 1000 rows and a statement at 2100
// parameters, so the real bound is whichever comes first: 2100 divided by the
// column count. Both are the server's, not a tuning choice.
const rowsPerInsert = 1000

// maxParams is the server's parameter limit per statement. sp_executesql's own
// two parameters count against it.
const maxParams = 2100 - 2

// CopyFrom bulk-loads rows.
//
// EMULATED, and this package says so rather than letting a performance claim
// depend on which adapter was passed — the same disclosure runtime/mydrv makes.
// It is a multi-row INSERT of up to a thousand rows per statement, so a
// thousand rows is ONE round trip rather than a thousand, which is the property
// storm's guarantee is about. It is not the same thing as bcp: TDS has a
// genuine bulk-load packet type, and using it means writing a second wire
// format and a column-metadata negotiation. That is the next piece of M10, not
// a missing one.
func (c *Conn) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	if len(cols) == 0 {
		return 0, protoErr("CopyFrom needs at least one column")
	}
	perStmt := maxParams / len(cols)
	if perStmt > rowsPerInsert {
		perStmt = rowsPerInsert
	}
	if perStmt < 1 {
		return 0, protoErr("a row of %d columns exceeds SQL Server's %d-parameter limit for "+
			"one statement", len(cols), maxParams)
	}

	var (
		total int64
		args  []any
		rows  int
		stmt  []byte
	)
	flush := func() error {
		if rows == 0 {
			return nil
		}
		n, err := c.Exec(ctx, string(stmt), args)
		if err != nil {
			return err
		}
		total += n
		args = args[:0]
		rows = 0
		return nil
	}
	build := func(n int) {
		stmt = stmt[:0]
		stmt = append(stmt, "INSERT INTO "...)
		stmt = append(stmt, identBracket(table)...)
		stmt = append(stmt, " ("...)
		for i, col := range cols {
			if i > 0 {
				stmt = append(stmt, ", "...)
			}
			stmt = append(stmt, identBracket(col)...)
		}
		stmt = append(stmt, ") VALUES "...)
		p := 1
		for r := 0; r < n; r++ {
			if r > 0 {
				stmt = append(stmt, ", "...)
			}
			stmt = append(stmt, '(')
			for i := range cols {
				if i > 0 {
					stmt = append(stmt, ", "...)
				}
				stmt = append(stmt, '@', 'p')
				stmt = strconv.AppendInt(stmt, int64(p), 10)
				p++
			}
			stmt = append(stmt, ')')
		}
	}

	for src.Next() {
		v := src.Values()
		if len(v) != len(cols) {
			return total, protoErr("a row has %d values for %d columns", len(v), len(cols))
		}
		args = append(args, v...)
		rows++
		if rows == perStmt {
			build(rows)
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := src.Err(); err != nil {
		return total, err
	}
	if rows > 0 {
		build(rows)
		if err := flush(); err != nil {
			return total, err
		}
	}
	return total, nil
}

// identBracket quotes an identifier. compile/mssql owns the spelling for
// GENERATED code; this is the driver's own, for the one statement it builds
// itself — and it is the same rule, because a driver that quoted differently
// would be a second answer to a settled question.
func identBracket(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '[')
	for i := 0; i < len(s); i++ {
		if s[i] == ']' {
			out = append(out, ']')
		}
		out = append(out, s[i])
	}
	return string(append(out, ']'))
}
