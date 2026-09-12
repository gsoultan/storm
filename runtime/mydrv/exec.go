package mydrv

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// Conn is one connection, and satisfies runtime.Executor.
//
// Not safe for concurrent use: one Conn is one socket and the protocol is
// strictly request/response. Use Pool for concurrent callers; a Pool hands out
// one Conn per statement and is itself a runtime.Executor.
type Conn struct {
	c   *conn
	cfg Config
	sts map[string]*list.Element // prepared-statement cache, keyed by SQL text
	lru *list.List               // cache keys, most recently used at the front

	// mu covers the two flags below, and is held across the kill so that a
	// statement finishing concurrently cannot read them mid-decision.
	mu     sync.Mutex
	killed bool // a watcher killed the statement now in flight
	broken bool // the socket is in an unknown state; the pool must not reuse it
}

// Open dials a server and authenticates.
//
// There is no DSN parser on purpose — a half-parsed DSN silently connecting
// somewhere unintended is worse than an explicit struct.
func Open(ctx context.Context, cfg Config) (*Conn, error) {
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("mydrv: Addr must be host:port: %w", err)
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	// The handshake predates any statement, so there is no query to kill and
	// nothing to fall back to: the socket deadline IS the cancellation here.
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	}
	c := newConn(nc)
	if err := c.handshake(cfg, host); err != nil {
		nc.Close()
		return nil, err
	}
	_ = nc.SetDeadline(time.Time{})
	return &Conn{c: c, cfg: cfg, sts: map[string]*list.Element{}, lru: list.New()}, nil
}

func (x *Conn) Close() error { return x.c.c.Close() }

// watch arranges for the statement about to run to be killed if ctx finishes
// first, and returns the function that ends the watch.
//
// A socket deadline alone would stop THIS process waiting and leave the server
// running the statement — a cancelled query would keep its locks and keep
// burning the server's CPU. So cancellation is a real KILL QUERY, sent from a
// second connection, which aborts the statement server-side and leaves this
// connection healthy enough to reuse. Breaking the socket is the fallback for
// when that second connection cannot be made.
//
// The returned function reports ctx.Err() when it was the watcher, not the
// server, that ended the statement — otherwise the caller would see MySQL's
// "Query execution was interrupted" and not know it was their own cancellation.
func (x *Conn) watch(ctx context.Context) func() error {
	if ctx.Done() == nil {
		return func() error { return nil }
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		x.mu.Lock()
		defer x.mu.Unlock()
		select {
		case <-done: // the statement finished while we were waking up
			return
		default:
		}
		if err := killQuery(x.cfg, x.c.id); err != nil {
			// No second connection to be had. Break the socket instead: it
			// stops this process waiting, and costs the connection, because
			// what arrives on it next is the tail of a statement nobody read.
			x.broken = true
			_ = x.c.c.SetDeadline(time.Unix(1, 0))
			return
		}
		x.killed = true
	}()
	return func() error {
		x.mu.Lock()
		defer x.mu.Unlock()
		close(done)
		if x.killed {
			x.killed = false
			return ctx.Err()
		}
		if x.broken {
			return ctx.Err()
		}
		return nil
	}
}

// killQuery aborts the statement running under thread id on a second
// connection. KILL QUERY, not KILL: it ends the statement, not the session.
func killQuery(cfg Config, id uint32) error {
	if id == 0 {
		return errors.New("mydrv: no server thread id for this connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()
	side, err := Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer side.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = side.c.c.SetDeadline(dl)
	}
	return side.c.query("KILL QUERY "+strconv.FormatUint(uint64(id), 10), nil)
}

// killTimeout bounds the side connection. A kill that cannot be delivered
// quickly is not worth waiting on — the fallback breaks the socket.
const killTimeout = 5 * time.Second

// entry is one cached prepared statement.
type entry struct {
	sql string
	s   *stmt
}

// prepared returns a cached prepared statement, preparing it if needed.
//
// The cache is BOUNDED and evicts least-recently-used, for two reasons. The
// server counts prepared statements too — max_prepared_stmt_count defaults to
// 16382, and hitting it fails every subsequent prepare on the whole server, not
// just this connection. And the key is SQL text, which is not a closed set: a
// query with an IN-list has one text per arity, so an unbounded map here is a
// map that grows with the caller's input.
func (x *Conn) prepared(sql string) (*stmt, error) {
	if el, ok := x.sts[sql]; ok {
		x.lru.MoveToFront(el)
		return el.Value.(*entry).s, nil
	}
	s, err := x.c.prepare(sql)
	if err != nil {
		return nil, classify(err)
	}
	x.sts[sql] = x.lru.PushFront(&entry{sql: sql, s: s})
	for x.lru.Len() > x.maxStmts() {
		back := x.lru.Back()
		e := back.Value.(*entry)
		x.lru.Remove(back)
		delete(x.sts, e.sql)
		// A failed close is not worth reporting to the caller — it happened on
		// behalf of a statement they did not run. It is worth attempting,
		// because the server holds the handle until it is told to let go.
		_ = e.s.close()
	}
	return x.sts[sql].Value.(*entry).s, nil
}

func (x *Conn) maxStmts() int {
	if x.cfg.MaxPreparedStmts > 0 {
		return x.cfg.MaxPreparedStmts
	}
	return DefaultMaxPreparedStmts
}

// DefaultMaxPreparedStmts is the per-connection prepared-statement cache size
// when Config.MaxPreparedStmts is zero. Generated code runs a small fixed set
// of statements, so this is sized for that and not for ad-hoc SQL.
const DefaultMaxPreparedStmts = 128

// once prepares, runs and closes a statement without caching it.
//
// For SQL whose TEXT varies with the data — a multi-row INSERT has one text per
// batch size — caching would fill the cache with statements that are never seen
// twice and evict the ones that are.
func (x *Conn) once(ctx context.Context, sql string, args []any) (int64, error) {
	stop := x.watch(ctx)
	var n int64
	s, err := x.c.prepare(sql)
	if err == nil {
		err = s.execAffected(args, &n)
		_ = s.close()
	}
	if cerr := stop(); cerr != nil {
		return 0, cerr
	}
	return n, classify(err)
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
	// The watch starts before the prepare, because a prepare is a round trip
	// too, and it ends on every path below — a watcher left running would kill
	// whatever this connection ran NEXT, which after a pool release is someone
	// else's statement.
	stop := x.watch(ctx)
	out := &rows{}
	s, err := x.prepared(sql)
	if err == nil {
		err = s.exec(args, func(cols [][]byte) error {
			row := make([][]byte, len(cols))
			for i, c := range cols {
				if c == nil {
					continue
				}
				// Copied because the packet buffer is reused and this result
				// set outlives the read — see the note on rows.
				row[i] = append([]byte(nil), c...)
			}
			out.vals = append(out.vals, row)
			return nil
		})
	}
	if cerr := stop(); cerr != nil {
		return nil, cerr
	}
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// Exec runs a statement and reports the rows it affected.
func (x *Conn) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	stop := x.watch(ctx)
	var n int64
	s, err := x.prepared(sql)
	if err == nil {
		err = s.execAffected(args, &n)
	}
	if cerr := stop(); cerr != nil {
		return 0, cerr
	}
	if code(err) == erUnsupportedPS && len(args) == 0 {
		return x.simple(ctx, sql)
	}
	return n, classify(err)
}

// simple runs a statement through COM_QUERY instead of the prepared protocol.
//
// MySQL's prepared protocol does not accept every statement — START
// TRANSACTION, LOCK TABLES, several SHOW forms — and answers 1295 for the ones
// it refuses. Exec falls back here for those, but only when there are NO
// arguments: the text protocol has nowhere to put them, and interpolating them
// into the SQL is the injection this driver exists to avoid.
func (x *Conn) simple(ctx context.Context, sql string) (int64, error) {
	stop := x.watch(ctx)
	var n int64
	x.c.onOK = func(p []byte) {
		v, _, _ := lenEncInt(p[1:])
		n = int64(v)
	}
	err := x.c.query(sql, nil)
	x.c.onOK = nil
	if cerr := stop(); cerr != nil {
		return 0, cerr
	}
	return n, classify(err)
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
	if _, err := x.once(ctx, b.String(), args); err != nil {
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
