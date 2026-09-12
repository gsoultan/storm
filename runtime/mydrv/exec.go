package mydrv

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// Conn is one connection, and satisfies runtime.Executor.
//
// Not safe for concurrent use: one Conn is one socket and the protocol is
// strictly request/response. A pool belongs here eventually; until it does,
// give each goroutine its own.
type Conn struct {
	c   *conn
	sts map[string]*stmt // prepared-statement cache, keyed by SQL text
}

// Open dials a server and authenticates.
//
// addr is host:port. There is no DSN parser yet on purpose — a half-parsed DSN
// silently connecting somewhere unintended is worse than an explicit argument.
func Open(ctx context.Context, addr, user, pass, db string) (*Conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := newConn(nc)
	if err := c.handshake(user, pass, db); err != nil {
		nc.Close()
		return nil, err
	}
	return &Conn{c: c, sts: map[string]*stmt{}}, nil
}

func (x *Conn) Close() error { return x.c.c.Close() }

// deadline applies the context's deadline to the socket.
//
// This is a DEADLINE, not a cancellation: it stops this process waiting, and
// the server keeps running the statement. A real driver sends COM_KILL_QUERY on
// another connection, which needs the pool this does not have yet.
func (x *Conn) deadline(ctx context.Context) func() {
	if dl, ok := ctx.Deadline(); ok {
		_ = x.c.c.SetDeadline(dl)
		return func() { _ = x.c.c.SetDeadline(time.Time{}) }
	}
	return func() {}
}

func (x *Conn) prepared(sql string) (*stmt, error) {
	if s, ok := x.sts[sql]; ok {
		return s, nil
	}
	s, err := x.c.prepare(sql)
	if err != nil {
		return nil, err
	}
	x.sts[sql] = s
	return s, nil
}

// rows buffers one result set's raw bytes.
//
// storm's contract is that RawValues is valid until the next Next, which the
// wire gives for free — the column slices point into a reused packet buffer. It
// is materialised here anyway, because the protocol is strictly
// request/response: holding a result set open holds the CONNECTION, and a
// generated plan loads relations while iterating a parent. Streaming would
// deadlock on the second query. A pooled driver streams; this one cannot yet,
// and says so rather than deadlocking.
type rows struct {
	vals [][][]byte
	i    int
	err  error
}

func (r *rows) Next() bool {
	if r.i >= len(r.vals) {
		return false
	}
	r.i++
	return true
}
func (r *rows) RawValues() [][]byte { return r.vals[r.i-1] }
func (r *rows) Close()              {}
func (r *rows) Err() error          { return r.err }

// Query runs a statement and returns its rows.
func (x *Conn) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	defer x.deadline(ctx)()
	s, err := x.prepared(sql)
	if err != nil {
		return nil, err
	}
	out := &rows{}
	err = s.exec(args, func(cols [][]byte) error {
		row := make([][]byte, len(cols))
		for i, c := range cols {
			if c == nil {
				continue
			}
			// Copied because the packet buffer is reused and this result set
			// outlives the read — see the note on rows.
			row[i] = append([]byte(nil), c...)
		}
		out.vals = append(out.vals, row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Exec runs a statement and reports the rows it affected.
func (x *Conn) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	defer x.deadline(ctx)()
	s, err := x.prepared(sql)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.execAffected(args, &n)
	return n, err
}

// ErrNoCopyProtocol is why CopyFrom is emulated.
//
// storm's port says an adapter whose driver has no copy protocol "must emulate
// it and say so in its package documentation. Silently degrading to 1,000 round
// trips would make a performance claim depend on which adapter was passed."
// MySQL has no COPY. Its nearest equivalent is a multi-row INSERT, which is one
// round trip but not the same wire path — no parse skipping, and the statement
// text grows with the batch.
var ErrNoCopyProtocol = errors.New(
	"mydrv: MySQL has no COPY protocol; CopyFrom is emulated with a multi-row INSERT, " +
		"which is one round trip but pays statement parsing that a real copy skips")

// CopyFrom emulates a bulk load with a multi-row INSERT.
func (x *Conn) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	defer x.deadline(ctx)()
	var b strings.Builder
	b.WriteString("INSERT INTO " + quote(table) + " (")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quote(c))
	}
	b.WriteString(") VALUES ")

	var args []any
	n := int64(0)
	for src.Next() {
		if n > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for i := range cols {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('?')
		}
		b.WriteByte(')')
		args = append(args, src.Values()...)
		n++
	}
	if err := src.Err(); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if _, err := x.Exec(ctx, b.String(), args); err != nil {
		return 0, err
	}
	return n, nil
}

// Batch runs each op in turn.
//
// Sequentially, not pipelined: MySQL's protocol is request/response and has no
// equivalent of PostgreSQL's extended-query pipeline. So this is N round trips
// wearing a batch's shape, and a caller counting round trips will find that
// out. Saying so is the same rule CopyFrom follows.
//
// The per-op error goes to the callback rather than ending the batch, matching
// the port: returning an error FROM the callback is what aborts.
func (x *Conn) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {
	defer x.deadline(ctx)()
	for i, op := range ops {
		var (
			r   runtime.Rows
			n   int64
			err error
		)
		if op.WantRows {
			r, err = x.Query(ctx, op.SQL, op.Args)
		} else {
			n, err = x.Exec(ctx, op.SQL, op.Args)
		}
		// The op's error is HANDED TO the callback rather than returned: the
		// caller decides whether one failure ends the batch, which is what
		// lets a unit-of-work report every failure rather than the first.
		if cbErr := each(i, r, n, err); cbErr != nil {
			return cbErr
		}
	}
	return nil
}

func quote(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
