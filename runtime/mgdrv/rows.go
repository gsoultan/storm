// Copyright (c) 2026 Gembit Soultan Shirazi
// SPDX-License-Identifier: MIT
//
// This file is part of storm, a compile-time ORM for PostgreSQL and Go,
// created and owned by Gembit Soultan Shirazi.
// Use of this source code is governed by the MIT License that can be
// found in the LICENSE file at the root of this repository.

package mgdrv

import (
	"context"

	"github.com/gsoultan/storm/runtime"
)

// rows walks a cursor's batches, fetching the next with getMore on the same
// connection, and presents each document as its fields in projection order.
type rows struct {
	c    *conn
	cols []string
	sess session
	done func()

	id   int64
	coll string

	// batch is the current batch's documents, copied out of the reply buffer
	// the connection reuses; pos is the offset of the next one.
	batch []byte
	pos   int
	doc   []byte

	vals    [][]byte
	scratch []byte
	err     error
	closed  bool
}

var _ runtime.Rows = (*rows)(nil)

// load takes a batch array (a BSON array document) for iteration.
func (r *rows) load(arr []byte) {
	r.batch = append(r.batch[:0], arr...)
	r.pos = 4
}

// loadOne takes a single document as a batch of one.
func (r *rows) loadOne(doc []byte) {
	b, at := startDoc(r.batch[:0])
	b = appendName(b, tDocument, "0")
	b = append(b, doc...)
	r.batch = endDoc(b, at)
	r.pos = 4
}

func (r *rows) Next() bool {
	if r.err != nil || r.closed {
		return false
	}
	for {
		if d, ok := r.nextInBatch(); ok {
			r.doc = d
			return true
		}
		if r.err != nil || r.id == 0 {
			return false
		}
		if !r.more() {
			return false
		}
	}
}

func (r *rows) nextInBatch() ([]byte, bool) {
	if r.batch == nil || r.pos >= len(r.batch)-1 {
		return nil, false
	}
	b := r.batch[r.pos:]
	t := b[0]
	i := 1
	for i < len(b) && b[i] != 0 {
		i++
	}
	i++
	sz := valueSize(t, b[i:])
	if t != tDocument || sz < 5 {
		r.err = errMalformed
		return nil, false
	}
	r.pos += i + sz
	return b[i : i+sz], true
}

// more fetches the next batch.
func (r *rows) more() bool {
	reply, err := r.c.run(context.Background(), func(b []byte) []byte {
		b = appendInt64(b, "getMore", r.id)
		b = appendStr(b, "collection", r.coll)
		if r.sess != nil {
			b = r.sess(b)
		}
		return appendStr(b, "$db", r.c.db)
	})
	if err != nil {
		r.err = err
		return false
	}
	_, cur, ok := lookup(reply, "cursor")
	if !ok {
		r.err = errMalformed
		return false
	}
	r.id = intField(cur, "id")
	_, next, _ := lookup(cur, "nextBatch")
	r.load(next)
	return true
}

// RawValues is the current document's fields, in projection order: each its
// BSON type byte and value bytes, nil when absent or null. Valid until Next.
func (r *rows) RawValues() [][]byte {
	if cap(r.vals) < len(r.cols) {
		r.vals = make([][]byte, len(r.cols))
	}
	r.vals = r.vals[:len(r.cols)]
	clear(r.vals)
	r.scratch = r.scratch[:0]
	// Offsets first and slices after the walk: appending may move scratch.
	var buf [32][2]int
	sp := buf[:0]
	if len(r.cols) > len(buf) {
		sp = make([][2]int, 0, len(r.cols))
	}
	sp = sp[:len(r.cols)]
	clear(sp)
	_ = elements(r.doc, func(t byte, name string, v []byte) bool {
		if t == tNull || t == tUndef {
			return true
		}
		for i, c := range r.cols {
			et, ev := t, v
			if c != name {
				// A dotted column — a composite key's "_id.tenant" — is a
				// field of the subdocument its head names.
				if len(c) <= len(name) || c[len(name)] != '.' || c[:len(name)] != name || t != tDocument {
					continue
				}
				var ok bool
				if et, ev, ok = path(v, c[len(name)+1:]); !ok || et == tNull || et == tUndef {
					continue
				}
			}
			at := len(r.scratch)
			r.scratch = append(r.scratch, et)
			r.scratch = append(r.scratch, ev...)
			sp[i] = [2]int{at, len(r.scratch)}
		}
		return true
	})
	for i, s := range sp {
		if s[1] > 0 {
			r.vals[i] = r.scratch[s[0]:s[1]:s[1]]
		}
	}
	return r.vals
}

func (r *rows) Err() error { return r.err }

// Close kills a cursor the caller stopped reading early and gives the
// connection back.
func (r *rows) Close() {
	if r.closed {
		return
	}
	if r.id != 0 && !r.c.broken {
		_, _ = r.c.run(context.Background(), func(b []byte) []byte {
			b = appendStr(b, "killCursors", r.coll)
			b = appendName(b, tArray, "cursors")
			b, at := startDoc(b)
			b = appendInt64(b, "0", r.id)
			b = endDoc(b, at)
			if r.sess != nil {
				b = r.sess(b)
			}
			return appendStr(b, "$db", r.c.db)
		})
		r.id = 0
	}
	r.closed = true
	r.finish()
}

func (r *rows) finish() {
	if r.done != nil {
		r.done()
		r.done = nil
	}
}

// path looks a dotted path up inside a document.
func path(doc []byte, p string) (byte, []byte, bool) {
	for {
		head, rest, more := cut(p)
		t, v, ok := lookup(doc, head)
		if !ok || !more {
			return t, v, ok
		}
		if t != tDocument {
			return 0, nil, false
		}
		doc, p = v, rest
	}
}

func cut(p string) (string, string, bool) {
	for i := 0; i < len(p); i++ {
		if p[i] == '.' {
			return p[:i], p[i+1:], true
		}
	}
	return p, "", false
}
