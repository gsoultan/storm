// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package caduceusdrv

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/caduceus/wire"
	"github.com/gsoultan/storm/v2/runtime"
)

// ErrPoolClosed is a Pool used after Close.
var ErrPoolClosed = errors.New("caduceusdrv: the pool is closed")

// ErrPoolExhausted is a wait for a connection that ran past AcquireTimeout:
// every connection stayed in use, which is most often rows left unclosed.
var ErrPoolExhausted = errors.New("caduceusdrv: no connection came free within AcquireTimeout")

// Pool is a pool of caduceus connections, and a runtime.DB: every generated
// package takes it, and StartTx begins a transaction on a connection of its
// own.
//
// A connection is taken for each statement and given back when its result
// is read, or for a transaction until it is settled. A connection given
// back with a transaction still open is unwound, which rolls it back; one
// in the middle of a result, closed or found dead is closed rather than
// kept. The pool opens connections as they are asked for, up to MaxConns,
// and checks each idle one as it hands it out, without a round trip.
type Pool struct {
	config PoolConfig
	slots  chan struct{}

	mu     sync.Mutex
	idle   []*member
	closed bool
}

var _ runtime.DB = (*Pool)(nil)

// NewPool parses dsn with ParseConfig and opens a pool on it.
func NewPool(ctx context.Context, dsn string) (*Pool, error) {
	c, err := ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return NewPoolConfig(ctx, c)
}

// NewPoolConfig opens a pool and one connection, so a database that is not
// there is found now rather than by the first query.
func NewPoolConfig(ctx context.Context, c PoolConfig) (*Pool, error) {
	c, err := c.resolve()
	if err != nil {
		return nil, err
	}
	c.MaxConns = c.maxConns()
	p := &Pool{config: c, slots: make(chan struct{}, c.MaxConns)}
	m, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	p.release(m)
	return p, nil
}

// Config returns the configuration each connection is opened with. It is
// the pool's own: change a Copy of it.
func (p *Pool) Config() *wire.Config { return p.config.Conn }

// MaxConns is the most connections the pool holds open at once.
func (p *Pool) MaxConns() int { return p.config.MaxConns }

// acquire takes a slot and a connection: an idle one that is still sound, or
// a new one.
func (p *Pool) acquire(ctx context.Context) (*member, error) {
	if err := p.slot(ctx); err != nil {
		return nil, err
	}
	now := time.Now()
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			<-p.slots
			return nil, ErrPoolClosed
		}
		var m *member
		if n := len(p.idle); n > 0 {
			m, p.idle[n-1] = p.idle[n-1], nil
			p.idle = p.idle[:n-1]
		}
		p.mu.Unlock()
		if m == nil {
			break
		}
		// CheckConn reads the socket without a round trip; a connection
		// back from its last statement less than idleProbeAfter ago is
		// taken on trust, which keeps a busy pool's hot path free of it.
		if m.stale(p.config, now) || (now.Sub(m.returned) >= idleProbeAfter && m.conn.CheckConn() != nil) {
			discard(m.conn)
			continue
		}
		return m, nil
	}
	conn, err := caduceus.ConnectConfig(ctx, p.config.Conn.Copy(), p.config.options())
	if err != nil {
		<-p.slots
		return nil, fmt.Errorf("caduceusdrv: connect: %w", err)
	}
	return &member{conn: conn, pool: p, born: now}, nil
}

// slot waits for room for one more connection in use: until ctx is done, or
// AcquireTimeout passes, which reports a pool that leaks connections rather
// than hanging on it.
func (p *Pool) slot(ctx context.Context) error {
	select {
	case p.slots <- struct{}{}:
		return nil
	default:
	}
	var timeout <-chan time.Time
	if wait := p.config.acquireTimeout(); wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case p.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timeout:
		return ErrPoolExhausted
	}
}

// idleProbeAfter is how long a connection sits idle before it is checked,
// without a round trip, for a server that hung up on it.
const idleProbeAfter = time.Second

// release gives m back: kept if it is sound and idle, closed otherwise.
func (p *Pool) release(m *member) {
	defer func() { <-p.slots }()
	if m.conn.IsClosed() || m.conn.Busy() {
		discard(m.conn)
		return
	}
	if m.conn.TxStatus() != 'I' {
		ctx, cancel := context.WithTimeout(context.Background(), unwindTimeout)
		err := m.conn.Unwind(ctx)
		cancel()
		if err != nil {
			discard(m.conn)
			return
		}
	}
	m.returned = time.Now()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		discard(m.conn)
		return
	}
	p.idle = append(p.idle, m)
	p.mu.Unlock()
}

// unwindTimeout bounds rolling back what a caller left open.
const unwindTimeout = 5 * time.Second

// discard closes a connection the pool will not keep.
func discard(c *caduceus.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.Close(ctx)
}

// once makes release safe to call more than once, for a transaction whose
// Commit and deferred Rollback both settle it.
func (p *Pool) once(m *member) func() {
	var done sync.Once
	return func() { done.Do(func() { p.release(m) }) }
}

func (p *Pool) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	m, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	return query(ctx, m.conn, sql, args, m)
}

func (p *Pool) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	m, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer p.release(m)
	return exec(ctx, m.conn, sql, args)
}

func (p *Pool) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	m, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer p.release(m)
	return copyFrom(ctx, m.conn, table, cols, src)
}

func (p *Pool) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	if len(ops) == 0 {
		return nil
	}
	m, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer p.release(m)
	return batch(ctx, m.conn, ops, each)
}

// StartTx begins a transaction on a connection that stays the
// transaction's until Commit or Rollback.
func (p *Pool) StartTx(ctx context.Context) (runtime.Tx, error) {
	return p.BeginTx(ctx, caduceus.TxOptions{})
}

// BeginTx is StartTx with the transaction's characteristics: an isolation
// level, read-only, deferrable. runtime.DB does not name them, so they live
// here rather than on the interface every adapter implements.
func (p *Pool) BeginTx(ctx context.Context, o caduceus.TxOptions) (runtime.Tx, error) {
	m, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	t, err := m.conn.BeginTx(ctx, o)
	if err != nil {
		p.release(m)
		return nil, classify(err)
	}
	return Tx{T: t, done: p.once(m)}, nil
}

// Acquire lends fn a connection of its own, for work that needs one
// connection throughout, such as a migration holding an advisory lock, and
// gives it back when fn returns.
func (p *Pool) Acquire(ctx context.Context, fn func(*caduceus.Conn) error) error {
	m, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer p.release(m)
	return fn(m.conn)
}

// Ping makes a round trip to the database.
func (p *Pool) Ping(ctx context.Context) error {
	return p.Acquire(ctx, func(c *caduceus.Conn) error { return c.Ping(ctx) })
}

// DatabaseSize reports the bytes the pool's database takes on disk, as
// pg_database_size counts them.
func (p *Pool) DatabaseSize(ctx context.Context) (int64, error) {
	var n int64
	err := p.Acquire(ctx, func(c *caduceus.Conn) error {
		return classify(c.QueryRow(ctx, "SELECT pg_database_size(current_database())").Scan(&n))
	})
	return n, err
}

// Close closes the idle connections, and each one in use as it is given
// back. Using the pool afterwards returns ErrPoolClosed.
func (p *Pool) Close() {
	p.mu.Lock()
	idle := p.idle
	p.idle, p.closed = nil, true
	p.mu.Unlock()
	for _, m := range idle {
		discard(m.conn)
	}
}

// QueryRow runs a statement expected to return at most one row, for the ad
// hoc question no model has words for. The connection goes back to the pool
// once the row is scanned.
func (p *Pool) QueryRow(ctx context.Context, sql string, in ...any) *Row {
	m, err := p.acquire(ctx)
	if err != nil {
		return &Row{err: err}
	}
	return &Row{r: m.conn.QueryRow(ctx, sql, args(in)...), release: m}
}
