package mydrv

// Pooling.
//
// One Conn is one socket and MySQL's protocol is strictly request/response, so
// a Conn cannot be shared. Pool is what makes the adapter usable from more than
// one goroutine, and it is also what makes cancellation work: killing a query
// needs a SECOND connection, which a single-connection driver does not have.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// ErrPoolClosed is returned by a Pool that has been closed.
var ErrPoolClosed = errors.New("mydrv: the pool is closed")

// DefaultAcquireTimeout bounds a wait for a pooled connection when
// Config.AcquireTimeout is zero.
//
// Long enough that a busy pool under a burst still serves; short enough that a
// LEAK is reported rather than hung on. Those are the two cases, and only the
// second is a bug.
const DefaultAcquireTimeout = 30 * time.Second

// ErrPoolExhausted is returned when no connection became free in time.
//
// Almost always a result set that was never closed: rows hold their connection
// until Close, so a missing one removes a connection from the pool for good.
// Raising MaxConns hides that for MaxConns more queries.
var ErrPoolExhausted = errors.New(
	"mydrv: no pooled connection became free — every connection is checked out, which " +
		"usually means a result set was not closed (rows hold their connection until Close)")

// DefaultMaxConns is the cap when Config.MaxConns is zero.
//
// Deliberately small. MySQL's per-connection memory is not free and its default
// max_connections is 151; a client library that defaults to "as many as you
// ask for" turns a traffic spike into a server that refuses everyone.
const DefaultMaxConns = 8

// Pool is a bounded set of connections, and satisfies runtime.Executor.
//
// Connections are opened on demand up to MaxConns and reused after. A
// connection whose statement had to be cancelled by breaking the socket is
// closed rather than reused: what arrives on it next is the tail of a result
// nobody read.
type Pool struct {
	cfg  Config
	idle chan *Conn
	sem  chan struct{} // one token per connection permitted to exist

	mu     sync.Mutex
	closed bool
}

// NewPool creates a pool and opens one connection, so that a bad address or a
// bad password is an error HERE rather than on the first query.
func NewPool(ctx context.Context, cfg Config) (*Pool, error) {
	n := cfg.MaxConns
	if n <= 0 {
		n = DefaultMaxConns
	}
	p := &Pool{
		cfg:  cfg,
		idle: make(chan *Conn, n),
		sem:  make(chan struct{}, n),
	}
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	p.release(c)
	return p, nil
}

// acquire takes an idle connection, or opens one, or waits for either.
func (p *Pool) acquire(ctx context.Context) (*Conn, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, ErrPoolClosed
	}
	// Prefer a warm connection: it has a prepared-statement cache, and storm's
	// generated code runs the same handful of statements over and over.
	select {
	case c := <-p.idle:
		return c, nil
	default:
	}
	wait, stop := p.waitFor()
	defer stop()
	select {
	case c := <-p.idle:
		return c, nil
	case p.sem <- struct{}{}:
		c, err := Open(ctx, p.cfg)
		if err != nil {
			<-p.sem
			return nil, err
		}
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-wait:
		return nil, ErrPoolExhausted
	}
}

// waitFor is the acquire deadline, or a channel that never fires when the
// caller asked to wait indefinitely.
func (p *Pool) waitFor() (<-chan time.Time, func()) {
	d := p.cfg.AcquireTimeout
	if d == 0 {
		d = DefaultAcquireTimeout
	}
	if d < 0 {
		return nil, func() {}
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// release returns a connection, or closes it and gives back its token.
//
// Giving the token back matters as much as returning the connection: a caller
// blocked in acquire is waiting on EITHER, so a discarded connection that kept
// its token would shrink the pool by one every time a query was cancelled the
// hard way.
func (p *Pool) release(c *Conn) {
	c.mu.Lock()
	broken := c.broken
	c.mu.Unlock()

	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()

	if !broken && !closed {
		select {
		case p.idle <- c:
			return
		default:
		}
	}
	_ = c.Close()
	<-p.sem
}

// Close closes every connection the pool holds. Connections checked out at the
// time are closed when they are released.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	for {
		select {
		case c := <-p.idle:
			_ = c.Close()
			<-p.sem
		default:
			return
		}
	}
}

// Query runs a statement on a pooled connection.
//
// The connection is held until the rows are CLOSED, because they stream. A
// caller who forgets to close leaks it — which is why the generated code closes
// with a defer, and why MaxConns should exceed the number of result sets a
// single request has open at once.
func (p *Pool) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	r, err := c.Query(ctx, sql, args)
	if err != nil {
		p.release(c)
		return nil, err
	}
	r.(*rows).rel = func() { p.release(c) }
	return r, nil
}

func (p *Pool) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer p.release(c)
	return c.Exec(ctx, sql, args)
}

// CopyFrom is emulated — see ErrNoCopyProtocol.
func (p *Pool) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer p.release(c)
	return c.CopyFrom(ctx, table, cols, src)
}

// Batch runs every op on ONE connection. Spreading them would break any op that
// depends on session state, and would report affected-row counts from
// connections the caller never asked about.
func (p *Pool) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {
	c, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer p.release(c)
	return c.Batch(ctx, ops, each)
}

// Tx is a transaction pinned to one connection, and satisfies runtime.Executor.
//
// This is ADR-0005's "a transaction is an Executor you were given": Begin,
// Commit and Rollback stay OUT of the port, the caller owns the transaction's
// lifetime, and every piece of generated code runs inside it unchanged because
// none of it knows what is behind the interface.
//
//	tx, err := pool.Begin(ctx)
//	defer tx.Rollback(ctx)
//	... generated calls against tx ...
//	tx.Commit(ctx)
//
// Pinning is not an optimisation here, it is the correctness requirement: BEGIN
// is session state, so a transaction run through the pool's own Exec would open
// on one connection and commit on another.
type Tx struct {
	p    *Pool
	c    *Conn
	done bool
}

// Begin starts a transaction on a connection taken from the pool, and returns
// it as a runtime.Tx so that a caller who holds a runtime.DB does not have to
// know which database is behind it. The concrete type is still *Tx and its
// fields were never exported, so nothing a caller could write against it is
// lost by naming the interface here.
func (p *Pool) Begin(ctx context.Context) (runtime.Tx, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	// Through the text protocol: MySQL's prepared protocol refuses START
	// TRANSACTION outright, and preparing what cannot be prepared would spend a
	// round trip to be told so.
	if _, err := c.simple(ctx, "START TRANSACTION"); err != nil {
		p.release(c)
		return nil, err
	}
	return &Tx{p: p, c: c}, nil
}

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
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {
	if t.done {
		return ErrTxDone
	}
	return t.c.Batch(ctx, ops, each)
}

// Commit ends the transaction and returns its connection to the pool.
func (t *Tx) Commit(ctx context.Context) error { return t.end(ctx, "COMMIT") }

// Rollback ends the transaction and returns its connection to the pool. It is
// safe to call after Commit, so that `defer tx.Rollback(ctx)` is the right
// shape.
func (t *Tx) Rollback(ctx context.Context) error {
	if t.done {
		return nil
	}
	return t.end(ctx, "ROLLBACK")
}

func (t *Tx) end(ctx context.Context, verb string) error {
	if t.done {
		return ErrTxDone
	}
	t.done = true
	_, err := t.c.simple(ctx, verb)
	if err != nil {
		// The connection's transaction state is now unknown. Handing it back
		// would give the next caller an open transaction.
		t.c.mu.Lock()
		t.c.broken = true
		t.c.mu.Unlock()
	}
	t.p.release(t.c)
	return err
}

// ErrTxDone is returned by a transaction used after it committed or rolled
// back. It IS runtime.ErrTxDone rather than a second error with the same
// meaning, so that errors.Is against the shared one holds for every adapter —
// which is what makes a helper written over runtime.Tx portable.
var ErrTxDone = runtime.ErrTxDone

var (
	_ runtime.Executor = (*Pool)(nil)
	_ runtime.Executor = (*Tx)(nil)
)
