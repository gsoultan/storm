package msdrv

import (
	"context"
	"errors"
	"sync"

	"github.com/gsoultan/storm/runtime"
)

// Conn is one connection. Not safe for concurrent use: TDS has no request
// multiplexing without MARS, and MARS is a second session on the same socket
// rather than pipelining — so one statement at a time is the protocol's own
// shape, and a Pool is how concurrency is served.
type Conn struct {
	x   *conn
	cfg Config

	// busy is set while a result set is open. The connection cannot carry
	// another statement until it is drained, and saying so is better than the
	// desynchronised stream that would otherwise follow.
	busy bool
	// bad marks a connection whose stream is no longer trustworthy, so the
	// pool discards it rather than handing it on.
	bad bool

	// bulk caches the column shape a bulk load declares, per table and column
	// list. The shape is read from the server, and reading it per load would
	// make a thousand-row COPY two round trips instead of one.
	bulk map[bulkKey][]column
}

// ErrRowsOpen is returned when a second statement is issued while a result set
// from the first is still open.
//
// The same refusal runtime/mydrv makes, for the same reason: the rows are being
// streamed off the socket, so issuing another statement would interleave two
// token streams on one connection and produce garbage for both. Close the rows
// first, or take a second connection from the pool.
var ErrRowsOpen = errors.New(
	"msdrv: a result set from a previous query is still open on this connection — close it " +
		"before issuing another statement, or use a Pool so the two get different connections")

// Open makes exactly one connection.
func Open(ctx context.Context, cfg Config) (*Conn, error) {
	x, err := dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Conn{x: x, cfg: cfg}, nil
}

// Close ends the connection.
func (c *Conn) Close() error { return c.x.c.Close() }

// watch cancels the running statement when ctx is done.
//
// TDS cancellation is an ATTENTION packet on the same connection, which is the
// one place this protocol is kinder than MySQL's: mydrv has to open a second
// connection and issue KILL QUERY, because a MySQL connection running a
// statement will not read anything else. Here the server watches for it.
//
// The server answers with a DONE carrying the attention bit, and that answer
// must still be read — the returned function does that, so the connection stays
// usable rather than being thrown away for having been cancelled.
func (c *Conn) watch(ctx context.Context) func() {
	if ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-ctx.Done():
			// Best effort. A failed attention means the connection is already
			// broken, which the read below will report with a better error than
			// this goroutine could.
			_ = c.x.attention()
		case <-done:
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// Query runs a statement that returns rows.
func (c *Conn) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	if c.busy {
		return nil, ErrRowsOpen
	}
	if err := c.send(ctx, sql, args); err != nil {
		return nil, err
	}
	r := &Rows{c: c}
	c.busy = true
	if err := r.header(); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// Exec runs a statement and returns the rows it affected.
func (c *Conn) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	if c.busy {
		return 0, ErrRowsOpen
	}
	if err := c.send(ctx, sql, args); err != nil {
		return 0, err
	}
	stop := c.watch(ctx)
	defer stop()

	var affected int64
	var first error
	var cols []column
	var scratch []byte
	var ar arena
	for {
		t, err := c.x.readByte()
		if err != nil {
			if errors.Is(err, errEndOfMessage) {
				break
			}
			c.bad = true
			return 0, err
		}
		switch t {
		case tokDone, tokDoneProc, tokDoneInProc:
			st, n, err := c.x.readDone()
			if err != nil {
				c.bad = true
				return 0, err
			}
			affected += countOf(t, st, n)
			if st&doneError != 0 && first == nil {
				first = protoErr("the server reported an error with no message")
			}
		case tokError:
			e, err := c.x.readMessage()
			if err != nil {
				c.bad = true
				return 0, err
			}
			if first == nil {
				first = e
			}
		case tokInfo:
			if _, err := c.x.readMessage(); err != nil {
				c.bad = true
				return 0, err
			}
		case tokEnvChange:
			if err := c.x.readEnvChange(); err != nil {
				c.bad = true
				return 0, err
			}
		case tokReturnStatus:
			if _, err := c.x.readU32(); err != nil {
				c.bad = true
				return 0, err
			}
		case tokReturnValue:
			if err := c.skipReturnValue(); err != nil {
				c.bad = true
				return 0, err
			}
		case tokColMetadata:
			// An Exec whose statement happens to return rows — an INSERT with
			// an OUTPUT clause reached through Exec rather than Query. The rows
			// are drained rather than refused: the count is what the caller
			// asked for and the stream still has to be consumed.
			cols, err = c.x.readColMetadata(cols)
			if err != nil {
				c.bad = true
				return 0, err
			}
		case tokRow, tokNBCRow:
			if err := c.skipRow(cols, t == tokNBCRow, &scratch, &ar); err != nil {
				c.bad = true
				return 0, err
			}
		case tokOrder:
			n, err := c.x.readU16()
			if err != nil {
				c.bad = true
				return 0, err
			}
			if err := c.x.skip(int(n)); err != nil {
				c.bad = true
				return 0, err
			}
		default:
			c.bad = true
			return 0, protoErr("unexpected token 0x%02X in a statement reply", t)
		}
		if !c.x.inMsg {
			break
		}
	}
	if first != nil {
		return 0, classify(first)
	}
	return affected, nil
}

// countOf is how many rows a DONE token contributes to the affected total.
//
// The STATEMENT's count, never the PROCEDURE's. A statement sent through
// sp_executesql produces DONEINPROC for itself and DONEPROC for the procedure
// that carried it, and the second repeats the first for a SELECT — so summing
// every token with the count bit reports a SELECT of one row as two, and a
// five-statement batch as ten. Only DONE and DONEINPROC are statements.
func countOf(tok byte, status uint16, n int64) int64 {
	if tok == tokDoneProc || status&doneCount == 0 {
		return 0
	}
	return n
}

// skipRow reads and discards a row.
func (c *Conn) skipRow(cols []column, nbc bool, scratch *[]byte, ar *arena) error {
	var nulls []byte
	if nbc {
		n := (len(cols) + 7) / 8
		b, err := c.x.slice(n, scratch)
		if err != nil {
			return err
		}
		nulls = b
	}
	ar.reset()
	for i := range cols {
		if nulls != nil && nulls[i/8]&(1<<(i%8)) != 0 {
			continue
		}
		if _, err := c.x.readValue(&cols[i], scratch, ar); err != nil {
			return err
		}
	}
	return nil
}

// skipReturnValue discards an output parameter, which sp_executesql emits for
// its own return value.
func (c *Conn) skipReturnValue() error {
	if _, err := c.x.readU16(); err != nil { // ordinal
		return err
	}
	if _, err := c.x.readBVarchar(); err != nil { // name
		return err
	}
	if _, err := c.x.readByte(); err != nil { // status
		return err
	}
	if _, err := c.x.readU32(); err != nil { // user type
		return err
	}
	if _, err := c.x.readU16(); err != nil { // flags
		return err
	}
	col, err := c.x.readTypeInfo()
	if err != nil {
		return err
	}
	var scratch []byte
	_, err = c.x.readRaw(&col, &scratch)
	return err
}

// Rows streams a result set.
//
// The values handed back by RawValues point into the connection's packet buffer
// or into a scratch arena reused per row, so they are valid until the next call
// to Next. That is the same contract runtime/pgxdrv's RawValues has, and it is
// what makes exposing a row cost nothing.
type Rows struct {
	c    *Conn
	cols []column
	vals [][]byte

	scratch []byte
	ar      arena

	// rowbuf holds the current row's values, copied out of the packet buffer.
	//
	// The copy is not optional, and finding out why cost a test. A sub-slice of
	// the packet buffer is valid only until the NEXT PACKET is read — and a row
	// with a wide column spans many packets, so by the time the last column is
	// read the first one's bytes have been overwritten by the tail of the
	// fourth. The values came back as fragments of a later string, which looks
	// like a decoder bug and is not one.
	//
	// So every value is appended here and the slices are rebuilt AFTER the row
	// is complete: appending may reallocate, and handing out slices of a buffer
	// that is still growing has exactly the same failure one packet lower down.
	//
	// It is one memmove per value into a buffer reused for the life of the
	// result set, which costs no allocation — the property the adapter's budget
	// actually rests on.
	rowbuf  []byte
	spans   []span
	nullbuf []byte

	err    error
	closed bool
	// shared says this result is one of several on the connection, sent as a
	// batch. It changes where the result ENDS: a lone reply ends with the
	// message, and a batched one ends at its own DONEPROC with the next op's
	// tokens still to come.
	shared bool
	// affected accumulates the row counts the DONE tokens carry, which is the
	// only place an UPDATE's count appears.
	affected int64
	// done is set when the stream has reached its final DONE, so Next stops
	// asking for tokens that are not coming.
	done bool
}

// header reads up to the first COLMETADATA, so the caller learns about a
// statement error before it starts iterating.
func (r *Rows) header() error {
	for {
		t, err := r.c.x.readByte()
		if err != nil {
			if errors.Is(err, errEndOfMessage) {
				r.done = true
				return r.err
			}
			r.c.bad = true
			return err
		}
		switch t {
		case tokColMetadata:
			cols, err := r.c.x.readColMetadata(r.cols)
			if err != nil {
				r.c.bad = true
				return err
			}
			r.cols = cols
			if cap(r.vals) < len(cols) {
				r.vals = make([][]byte, len(cols))
			}
			r.vals = r.vals[:len(cols)]
			return r.err
		case tokError:
			e, err := r.c.x.readMessage()
			if err != nil {
				r.c.bad = true
				return err
			}
			if r.err == nil {
				r.err = classify(e)
			}
		case tokInfo:
			if _, err := r.c.x.readMessage(); err != nil {
				r.c.bad = true
				return err
			}
		case tokEnvChange:
			if err := r.c.x.readEnvChange(); err != nil {
				r.c.bad = true
				return err
			}
		case tokReturnStatus:
			if _, err := r.c.x.readU32(); err != nil {
				r.c.bad = true
				return err
			}
		case tokReturnValue:
			if err := r.c.skipReturnValue(); err != nil {
				r.c.bad = true
				return err
			}
		case tokOrder:
			n, err := r.c.x.readU16()
			if err != nil {
				r.c.bad = true
				return err
			}
			if err := r.c.x.skip(int(n)); err != nil {
				r.c.bad = true
				return err
			}
		case tokDone, tokDoneProc, tokDoneInProc:
			st, n, err := r.c.x.readDone()
			if err != nil {
				r.c.bad = true
				return err
			}
			r.affected += countOf(t, st, n)
			if r.shared && t == tokDoneProc {
				r.done = true
				return r.err
			}
			// A statement that returned no result set at all. Not an error —
			// an UPDATE reached through Query has none — so the iteration ends
			// immediately with whatever error the stream carried.
			if !r.c.x.inMsg {
				r.done = true
				return r.err
			}
		default:
			r.c.bad = true
			return protoErr("unexpected token 0x%02X before the first row", t)
		}
		if !r.c.x.inMsg {
			r.done = true
			return r.err
		}
	}
}

// Next advances to the next row.
func (r *Rows) Next() bool {
	if r.done || r.closed || r.err != nil {
		return false
	}
	for {
		t, err := r.c.x.readByte()
		if err != nil {
			if errors.Is(err, errEndOfMessage) {
				r.done = true
				return false
			}
			r.c.bad = true
			r.err = err
			return false
		}
		switch t {
		case tokRow, tokNBCRow:
			if err := r.readRow(t == tokNBCRow); err != nil {
				r.c.bad = true
				r.err = err
				return false
			}
			return true
		case tokDone, tokDoneProc, tokDoneInProc:
			st, n, err := r.c.x.readDone()
			if err != nil {
				r.c.bad = true
				r.err = err
				return false
			}
			r.affected += countOf(t, st, n)
			if r.shared && t == tokDoneProc {
				// The end of THIS op, whatever the "more results" bit says.
				// In a batched request the server sets that bit on every call
				// but the last — it means "more is coming in this MESSAGE",
				// which is true and is not this result's business. Reading it
				// as "this result continues" makes the first op swallow all
				// five replies and the second block on a packet nobody will
				// send.
				r.done = true
				return false
			}
			if st&doneAttn != 0 {
				// The statement was cancelled. The stream is clean — that is
				// what the attention handshake is for — so the connection
				// survives and the caller learns why.
				if r.err == nil {
					r.err = context.Canceled
				}
			}
			if st&doneMore == 0 && !r.c.x.inMsg {
				r.done = true
				return false
			}
		case tokColMetadata:
			// A second result set. storm's generated code reads one, so this is
			// only reachable through storm.SQL — the metadata is adopted and
			// iteration continues, which is more useful than refusing.
			cols, err := r.c.x.readColMetadata(r.cols)
			if err != nil {
				r.c.bad = true
				r.err = err
				return false
			}
			r.cols = cols
			if cap(r.vals) < len(cols) {
				r.vals = make([][]byte, len(cols))
			}
			r.vals = r.vals[:len(cols)]
		case tokError:
			e, err := r.c.x.readMessage()
			if err != nil {
				r.c.bad = true
				r.err = err
				return false
			}
			if r.err == nil {
				r.err = classify(e)
			}
		case tokInfo:
			if _, err := r.c.x.readMessage(); err != nil {
				r.c.bad = true
				r.err = err
				return false
			}
		case tokEnvChange:
			if err := r.c.x.readEnvChange(); err != nil {
				r.c.bad = true
				r.err = err
				return false
			}
		case tokReturnStatus:
			if _, err := r.c.x.readU32(); err != nil {
				r.c.bad = true
				r.err = err
				return false
			}
		case tokReturnValue:
			if err := r.c.skipReturnValue(); err != nil {
				r.c.bad = true
				r.err = err
				return false
			}
		case tokOrder:
			n, err := r.c.x.readU16()
			if err != nil {
				r.c.bad = true
				r.err = err
				return false
			}
			if err := r.c.x.skip(int(n)); err != nil {
				r.c.bad = true
				r.err = err
				return false
			}
		default:
			r.c.bad = true
			r.err = protoErr("unexpected token 0x%02X in a result set", t)
			return false
		}
		if !r.c.x.inMsg {
			r.done = true
			return false
		}
	}
}

// readRow fills vals from one ROW or NBCROW token.
//
// NBCROW is the null-bitmap form, and it is what the server sends for any row
// with a null in it: the nulls occupy a bit each and carry NO value bytes. A
// client that reads it as a plain ROW reads the next column's bytes as this
// one's and every value after it is garbage.
func (r *Rows) readRow(nbc bool) error {
	var nulls []byte
	if nbc {
		n := (len(r.cols) + 7) / 8
		b, err := r.c.x.slice(n, &r.scratch)
		if err != nil {
			return err
		}
		// Copied, because the bitmap is read from the packet buffer and the
		// very next value may pull the packet out from under it.
		if cap(r.nullbuf) < n {
			r.nullbuf = make([]byte, n)
		}
		r.nullbuf = r.nullbuf[:n]
		copy(r.nullbuf, b)
		nulls = r.nullbuf
	}
	r.ar.reset()
	r.rowbuf = r.rowbuf[:0]
	if cap(r.spans) < len(r.cols) {
		r.spans = make([]span, len(r.cols))
	}
	r.spans = r.spans[:len(r.cols)]
	for i := range r.cols {
		if nulls != nil && nulls[i/8]&(1<<(i%8)) != 0 {
			r.spans[i] = span{null: true}
			continue
		}
		v, err := r.c.x.readValue(&r.cols[i], &r.scratch, &r.ar)
		if err != nil {
			return err
		}
		if v == nil {
			r.spans[i] = span{null: true}
			continue
		}
		r.spans[i] = span{off: len(r.rowbuf), n: len(v)}
		r.rowbuf = append(r.rowbuf, v...)
	}
	// Only now, with the buffer's address settled.
	for i := range r.spans {
		if r.spans[i].null {
			r.vals[i] = nil
			continue
		}
		s := r.spans[i]
		r.vals[i] = r.rowbuf[s.off : s.off+s.n : s.off+s.n]
	}
	return nil
}

// span is where one value landed in rowbuf. A separate null flag rather than a
// zero length, because an empty string is not a NULL and conflating them is how
// an empty column becomes a missing one.
type span struct {
	off, n int
	null   bool
}

// RawValues is the current row's columns, as bytes. See the note on Rows for
// the lifetime, and the one at the top of types.go for what "raw" means for the
// four families that cannot be handed over as the wire carries them.
func (r *Rows) RawValues() [][]byte { return r.vals }

// Err reports the first error the stream carried.
func (r *Rows) Err() error { return r.err }

// Close drains the rest of the reply and frees the connection.
//
// Draining rather than closing: the connection is only reusable once the
// server's reply is consumed, and the alternative is a new TCP handshake and a
// new login for every result a caller stops reading early.
func (r *Rows) Close() {
	if r.closed {
		return
	}
	r.closed = true
	if !r.done && !r.c.bad {
		if err := r.c.x.drain(); err != nil {
			r.c.bad = true
		}
	}
	r.c.busy = false
}

// Columns reports the result's column names, for a caller reading a raw query
// whose shape it does not know at build time.
func (r *Rows) Columns() []string {
	out := make([]string, len(r.cols))
	for i := range r.cols {
		out[i] = r.cols[i].name
	}
	return out
}
