// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

// Package mgdrv is storm's MongoDB client: OP_MSG over TCP or TLS,
// SCRAM-SHA-256, a bounded pool, and transactions — stdlib only, so the
// driver dependency ADR-0004 expected (mongo-go-driver) is not one.
//
// It implements runtime.Executor. The "SQL" a generated package hands it is a
// command document written as JSON with numbered holes — `{"$arg":1}` — which
// compile/mongo wrote at generate time. The text is compiled to BSON once per
// distinct command and bound per call, so a warm statement costs an encode and
// a round trip, never a parse.
//
// Results come back in the byte shape runtime.Rows promises: one value per
// field, in the order the command's projection names them, each as its BSON
// type byte followed by the value's bytes. runtime/bsondec decodes them. A
// field that is absent or null is nil, which is NULL.
//
// What the port asks for and MongoDB lacks is EMULATED, and said so here
// rather than discovered:
//
//   - Batch sends its statements one after another, one round trip each.
//     OP_MSG has no pipelining a client can rely on for acknowledged writes.
//   - CopyFrom is the insert command with many documents, one round trip per
//     thousand — not a separate wire path the way COPY is.
//   - Cancellation abandons the connection mid-reply; the server is not
//     told. A cancelled read is harmless; a cancelled write may still apply,
//     exactly as it may when the network drops.
package mgdrv

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// Pool is a bounded set of connections to one server.
type Pool struct {
	cfg  Config
	sem  chan struct{}
	mu   sync.Mutex
	idle []*conn
	shut bool
}

var (
	_ runtime.Executor = (*Pool)(nil)
	_ runtime.DB       = (*Pool)(nil)
)

// ErrClosed is any use of a closed pool.
var ErrClosed = errors.New("mgdrv: the pool is closed")

// Open connects once, so a bad address or a wrong password fails here rather
// than on the first query, and returns the pool.
func Open(ctx context.Context, cfg Config) (*Pool, error) {
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 10
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.Database == "" {
		return nil, errors.New("mgdrv: Config.Database is required")
	}
	p := &Pool{cfg: cfg, sem: make(chan struct{}, cfg.MaxConns)}
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	p.release(c)
	return p, nil
}

// OpenURI is Open with a mongodb:// connection string.
func OpenURI(ctx context.Context, uri string) (*Pool, error) {
	cfg, err := ParseURI(uri)
	if err != nil {
		return nil, err
	}
	return Open(ctx, cfg)
}

// Close closes the idle connections; one in use closes when released.
func (p *Pool) Close() {
	p.mu.Lock()
	p.shut = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, c := range idle {
		c.close()
	}
}

func (p *Pool) acquire(ctx context.Context) (*conn, error) {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.mu.Lock()
	if p.shut {
		p.mu.Unlock()
		<-p.sem
		return nil, ErrClosed
	}
	if n := len(p.idle); n > 0 {
		c := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()
	dctx, cancel := context.WithTimeout(ctx, p.cfg.DialTimeout)
	defer cancel()
	c, err := dial(dctx, &p.cfg)
	if err != nil {
		<-p.sem
		return nil, err
	}
	return c, nil
}

func (p *Pool) release(c *conn) {
	p.mu.Lock()
	if c.broken || p.shut {
		p.mu.Unlock()
		c.close()
	} else {
		p.idle = append(p.idle, c)
		p.mu.Unlock()
	}
	<-p.sem
}

// Query runs a command whose result is documents: a find, an aggregate or a
// findAndModify.
func (p *Pool) Query(ctx context.Context, cmd string, args []any) (runtime.Rows, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	r, err := query(ctx, c, cmd, args, nil, func() { p.release(c) })
	if err != nil {
		p.release(c)
		return nil, err
	}
	return r, nil
}

// Exec runs a command and reports how many documents it affected: inserted,
// matched by an update, deleted, or found by a findAndModify. Any other
// command reports zero.
func (p *Pool) Exec(ctx context.Context, cmd string, args []any) (int64, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer p.release(c)
	return exec(ctx, c, cmd, args, nil)
}

// Batch runs ops in order, one round trip each — see the package comment.
func (p *Pool) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {
	return batch(ctx, p, ops, each)
}

// CopyFrom inserts every row src yields into the collection named by table,
// a thousand documents per round trip. cols are the field names.
func (p *Pool) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer p.release(c)
	return copyFrom(ctx, c, table, cols, src, nil)
}

// StartTx starts a transaction on a pinned connection. MongoDB starts it
// lazily, with the first statement, so a standalone server's refusal arrives
// there.
func (p *Pool) StartTx(ctx context.Context) (runtime.Tx, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	c.txnNum++
	return &tx{p: p, c: c}, nil
}

// ---- shared by the pool and a transaction ----

// session writes a transaction's fields into a command, or nothing outside one.
type session func(b []byte) []byte

func send(ctx context.Context, c *conn, text string, args []any, sess session) (*template, []byte, error) {
	t, err := compiled(text)
	if err != nil {
		return nil, nil, err
	}
	reply, err := c.runErr(ctx, func(b []byte) ([]byte, error) {
		b, err := t.encode(b, args)
		if err != nil {
			return b, err
		}
		if sess != nil {
			b = sess(b)
		}
		return appendStr(b, "$db", c.db), nil
	})
	if err != nil {
		return nil, nil, err
	}
	if err := writeError(reply); err != nil {
		return nil, nil, err
	}
	return t, reply, nil
}

func exec(ctx context.Context, c *conn, text string, args []any, sess session) (int64, error) {
	t, reply, err := send(ctx, c, text, args, sess)
	if err != nil {
		return 0, err
	}
	switch t.cmd {
	case "insert", "update", "delete":
		return intField(reply, "n"), nil
	case "findAndModify":
		if _, v, ok := lookup(reply, "lastErrorObject"); ok {
			return intField(v, "n"), nil
		}
	}
	return 0, nil
}

func query(ctx context.Context, c *conn, text string, args []any, sess session, done func()) (runtime.Rows, error) {
	t, reply, err := send(ctx, c, text, args, sess)
	if err != nil {
		return nil, err
	}
	if len(t.cols) == 0 {
		return nil, errors.New("mgdrv: " + t.cmd + " names no fields to read — a Query needs a " +
			"projection, a findAndModify's fields, or an aggregation ending in $project or $count")
	}
	r := &rows{c: c, cols: t.cols, sess: sess, done: done}
	switch t.cmd {
	case "find", "aggregate":
		_, cur, ok := lookup(reply, "cursor")
		if !ok {
			return nil, errMalformed
		}
		r.id = intField(cur, "id")
		r.coll = collOf(strField(cur, "ns"))
		_, batch, _ := lookup(cur, "firstBatch")
		r.load(batch)
	case "findAndModify":
		if typ, v, ok := lookup(reply, "value"); ok && typ == tDocument {
			r.loadOne(v)
		}
	default:
		return nil, errors.New("mgdrv: " + t.cmd + " does not return documents; use Exec")
	}
	if r.id == 0 {
		// Everything arrived in the first reply, so the connection is
		// free now rather than when the caller closes the rows.
		r.finish()
	}
	return r, nil
}

func collOf(ns string) string {
	for i := 0; i < len(ns); i++ {
		if ns[i] == '.' {
			return ns[i+1:]
		}
	}
	return ns
}

func batch(ctx context.Context, ex runtime.Executor, ops []runtime.BatchOp,
	each func(int, runtime.Rows, int64, error) error) error {
	var first error
	for i, op := range ops {
		var (
			rs  runtime.Rows
			n   int64
			err error
		)
		if op.WantRows {
			rs, err = ex.Query(ctx, op.SQL, op.Args)
		} else {
			n, err = ex.Exec(ctx, op.SQL, op.Args)
		}
		if each == nil {
			if rs != nil {
				rs.Close()
			}
			if err != nil && first == nil {
				first = err
			}
			continue
		}
		cbErr := each(i, rs, n, err)
		if rs != nil {
			rs.Close()
		}
		if cbErr != nil {
			return cbErr
		}
	}
	return first
}

const copyChunk = 1000

func copyFrom(ctx context.Context, c *conn, coll string, cols []string, src runtime.CopySource, sess session) (int64, error) {
	var total int64
	more := src.Next()
	for more {
		reply, err := c.runErr(ctx, func(b []byte) ([]byte, error) {
			b = appendStr(b, "insert", coll)
			b = appendBool(b, "ordered", true)
			b = appendName(b, tArray, "documents")
			b, arr := startDoc(b)
			for n := 0; more && n < copyChunk; n++ {
				b = appendName(b, tDocument, argName(n))
				var d int
				b, d = startDoc(b)
				var err error
				if b, err = appendFields(b, cols, src.Values()); err != nil {
					return b, errors.New("mgdrv: CopyFrom: " + err.Error())
				}
				b = endDoc(b, d)
				more = src.Next()
			}
			b = endDoc(b, arr)
			if sess != nil {
				b = sess(b)
			}
			return appendStr(b, "$db", c.db), nil
		})
		if err != nil {
			return total, err
		}
		total += intField(reply, "n")
		if err := writeError(reply); err != nil {
			return total, err
		}
	}
	return total, src.Err()
}
