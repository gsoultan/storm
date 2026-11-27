// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package pgdrv

// Pooling.
//
// A caduceus connection is one socket and belongs to one goroutine at a time,
// so Pool is what makes the adapter usable from many. It is small on purpose:
// a bounded set of connections opened on demand and reused after, a wait with
// a deadline when all of them are out, and a connection that cannot be trusted
// — closed by a cancelled statement, left busy or left in a transaction —
// closed rather than handed to the next caller.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gsoultan/caduceus"
	"github.com/gsoultan/caduceus/wire"
	"github.com/gsoultan/storm/runtime"
)

// ErrPoolClosed is returned by a Pool that has been closed.
var ErrPoolClosed = errors.New("pgdrv: the pool is closed")

// ErrPoolExhausted is returned when no connection became free in time.
//
// Almost always a result set that was never closed: rows hold their connection
// until Close, so a missing one removes a connection from the pool for good.
// Raising MaxConns hides that for MaxConns more queries.
var ErrPoolExhausted = errors.New(
	"pgdrv: no pooled connection became free — every connection is checked out, which " +
		"usually means a result set or a transaction was not closed")

// DefaultMaxConns is the cap when Config.MaxConns is zero.
const DefaultMaxConns = 16

// DefaultAcquireTimeout bounds a wait for a pooled connection when
// Config.AcquireTimeout is zero: long enough that a busy pool under a burst
// still serves, short enough that a leak is reported rather than hung on.
const DefaultAcquireTimeout = 30 * time.Second

// idleProbeAfter is how long a connection sits idle before it is checked,
// without a round trip, for a server that hung up on it.
const idleProbeAfter = time.Second

// Pool is a bounded set of connections, and satisfies runtime.DB.
type Pool struct {
	cfg  *wire.Config
	opt  caduceus.Options
	wait time.Duration
	idle chan *pooled
	sem  chan struct{} // one token per connection permitted to exist

	mu     sync.Mutex
	closed bool
}

// NewPool opens a pool on dsn with the defaults.
func NewPool(ctx context.Context, dsn string) (*Pool, error) {
	return NewPoolConfig(ctx, Config{DSN: dsn})
}

// NewPoolConfig opens a pool and one connection, so that a bad address or a
// bad password is an error HERE rather than on the first query.
func NewPoolConfig(ctx context.Context, cfg Config) (*Pool, error) {
	parsed, err := wire.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, err
	}
	if cfg.Database != "" {
		parsed.Database = cfg.Database
	}
	if len(cfg.RuntimeParams) > 0 {
		if parsed.RuntimeParams == nil {
			parsed.RuntimeParams = map[string]string{}
		}
		for k, v := range cfg.RuntimeParams {
			parsed.RuntimeParams[k] = v
		}
	}
	n := cfg.MaxConns
	if n <= 0 {
		n = DefaultMaxConns
	}
	wait := cfg.AcquireTimeout
	if wait == 0 {
		wait = DefaultAcquireTimeout
	}
	p := &Pool{
		cfg:  parsed,
		opt:  cfg.Options.caduceus(),
		wait: wait,
		idle: make(chan *pooled, n),
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
func (p *Pool) acquire(ctx context.Context) (*pooled, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, ErrPoolClosed
	}
	for {
		select {
		case c := <-p.idle:
			if p.usable(c) {
				return c, nil
			}
			continue
		default:
		}
		break
	}
	var timeout <-chan time.Time
	if p.wait > 0 {
		t := time.NewTimer(p.wait)
		defer t.Stop()
		timeout = t.C
	}
	for {
		select {
		case c := <-p.idle:
			if p.usable(c) {
				return c, nil
			}
		case p.sem <- struct{}{}:
			c, err := caduceus.ConnectConfig(ctx, p.cfg.Copy(), p.opt)
			if err != nil {
				<-p.sem
				return nil, err
			}
			return &pooled{c: c}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout:
			return nil, ErrPoolExhausted
		}
	}
}

// usable vets an idle connection before it is handed out, and discards it —
// token included — when the server has dropped it in the meantime. The check
// reads the socket and makes no round trip, so a failover or a restart costs
// no failed statement for a connection that sat idle across it.
func (p *Pool) usable(c *pooled) bool {
	if time.Since(c.idleSince) < idleProbeAfter || c.c.CheckConn() == nil {
		return true
	}
	p.discard(c)
	return false
}

// release returns a connection, or closes it and gives back its token.
//
// A connection is reused only when it is open, has nothing unread on it, and
// is outside any transaction: a cancelled statement closes its connection, and
// a transaction its caller forgot would otherwise be inherited by the next.
func (p *Pool) release(c *pooled) {
	ok := !c.c.IsClosed() && !c.c.Busy() && c.c.TxStatus() == wire.TxIdle
	p.mu.Lock()
	if ok && !p.closed {
		c.idleSince = time.Now()
		select {
		case p.idle <- c:
			p.mu.Unlock()
			return
		default:
		}
	}
	p.mu.Unlock()
	p.discard(c)
}

func (p *Pool) discard(c *pooled) {
	_ = c.c.Close(context.Background())
	<-p.sem
}

// Close closes every idle connection. Connections checked out at the time are
// closed when they are given back.
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
			p.discard(c)
		default:
			return
		}
	}
}

// Query runs a statement on a pooled connection, which is held until the rows
// are CLOSED, because they stream.
func (p *Pool) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	return query(ctx, c.c, sql, args, func() { p.release(c) })
}

func (p *Pool) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer p.release(c)
	return exec(ctx, c.c, sql, args)
}

func (p *Pool) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer p.release(c)
	return copyFrom(ctx, c.c, table, cols, src)
}

// Batch runs every op on ONE connection, in one round trip.
func (p *Pool) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	if len(ops) == 0 {
		return nil
	}
	c, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer p.release(c)
	return batch(ctx, c.c, ops, each)
}

// StartTx starts a transaction on a connection taken from the pool, which it
// holds until Commit or Rollback.
func (p *Pool) StartTx(ctx context.Context) (runtime.Tx, error) {
	return p.BeginTx(ctx, caduceus.TxOptions{})
}

// BeginTx is StartTx with the transaction's characteristics: an isolation
// level, read-only, deferrable. pgx had this as pool.BeginTx and storm's
// transaction contract never named it, so it lives here rather than on
// runtime.DB, where every other adapter would have to grow one.
func (p *Pool) BeginTx(ctx context.Context, o caduceus.TxOptions) (runtime.Tx, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	t, err := c.c.BeginTx(ctx, o)
	if err != nil {
		p.release(c)
		return nil, classify(err)
	}
	return &Tx{T: t, release: func() { p.release(c) }}, nil
}

// Acquire takes a connection out of the pool for state that lives on a
// session, such as an advisory lock: wrap it in Conn, and give it back with
// the function returned, which may be called once.
func (p *Pool) Acquire(ctx context.Context) (*caduceus.Conn, func(), error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	return c.c, func() { once.Do(func() { p.release(c) }) }, nil
}

// QueryRow runs a statement expected to return at most one row, for the ad hoc
// question no model has words for. The connection goes back to the pool once
// the row is scanned.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) *Row {
	c, err := p.acquire(ctx)
	if err != nil {
		return &Row{err: err}
	}
	return &Row{r: c.c.QueryRow(ctx, sql, params(args)...), release: func() { p.release(c) }}
}

// DatabaseSize reports the bytes the pool's database takes on disk, as
// pg_database_size counts them, for a usage or health page.
func (p *Pool) DatabaseSize(ctx context.Context) (int64, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer p.release(c)
	var n int64
	if err := c.c.QueryRow(ctx, "SELECT pg_database_size(current_database())").Scan(&n); err != nil {
		return 0, classify(err)
	}
	return n, nil
}

var _ runtime.DB = (*Pool)(nil)
