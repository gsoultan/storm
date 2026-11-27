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
	"github.com/gsoultan/storm/runtime"
)

// ErrPoolClosed is a Pool used after Close.
var ErrPoolClosed = errors.New("caduceusdrv: the pool is closed")

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
	if c.Conn == nil {
		return nil, errors.New("caduceusdrv: PoolConfig.Conn is nil")
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
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
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
		if m.stale(p.config, now) || m.conn.CheckConn() != nil {
			discard(m.conn)
			continue
		}
		return m, nil
	}
	conn, err := caduceus.ConnectConfig(ctx, p.config.Conn.Copy(), Options())
	if err != nil {
		<-p.slots
		return nil, fmt.Errorf("caduceusdrv: connect: %w", err)
	}
	return &member{conn: conn, born: now}, nil
}

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
	return query(ctx, m.conn, sql, args, p.once(m))
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
	m, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	t, err := m.conn.Begin(ctx)
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
