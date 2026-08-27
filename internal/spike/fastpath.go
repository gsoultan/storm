package spike

import (
	"context"
	"encoding/binary"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MEASURED AND REJECTED (2026-08-23) — kept so the result can be re-verified.
// See bench/RESULTS.md "Optimisation pass". Bypassing pgx bought ~4% fewer
// bytes, MORE allocations (22 vs 16 under concurrency), and was 10% SLOWER
// under load. The 5-allocation floor is pgconn's, not pgx's, so removing the
// type map removes nothing. Do not adopt this without new evidence.
//
// The fast path talks to pgconn directly instead of pgx.Query.
//
// pgx's value-add is its type map: turning driver values into Go values via
// Scan. A compiler-based ORM generates its own decoders, so that machinery is
// dead weight — it costs 5 allocations and 404 B per query before we do
// anything. Going one layer down removes it, and removes the []any argument
// slice with it: parameters are encoded straight to wire bytes.

// binFormats is all-binary; for text columns binary and text encodings are the
// same bytes, so one static array covers every parameter and result column.
var binFormats = [16]int16{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}

// pbuf holds wire-encoded parameters. Fixed arrays per slot, so nothing
// allocates; string parameters alias the caller's bytes with no copy at all.
type pbuf struct {
	vals  [][]byte
	org   [16]byte
	age   [4]byte
	since [8]byte
	limit [8]byte // LIMIT $n is inferred as bigint, not int4
}

var pbufs = sync.Pool{
	New: func() any { return &pbuf{vals: make([][]byte, 0, nFilters+1)} },
}

// noCopyBytes views a string's bytes without copying. Safe here: they are only
// read, and only for the duration of the call.
func noCopyBytes(s string) []byte {
	if len(s) == 0 {
		return []byte{}
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

func putI32(dst *[4]byte, v int32) []byte {
	binary.BigEndian.PutUint32(dst[:], uint32(v))
	return dst[:]
}

func putI64(dst *[8]byte, v int64) []byte {
	binary.BigEndian.PutUint64(dst[:], uint64(v))
	return dst[:]
}

func putTS(dst *[8]byte, t time.Time) []byte {
	binary.BigEndian.PutUint64(dst[:], uint64(t.Sub(pgEpoch)/time.Microsecond))
	return dst[:]
}

// encode writes this query's arguments as Postgres wire values.
func (q Query) encode(b *pbuf) [][]byte {
	v := b.vals[:0]
	if q.mask&fOrg != 0 {
		b.org = q.org
		v = append(v, b.org[:])
	}
	if q.mask&fEmail != 0 {
		v = append(v, noCopyBytes(q.email))
	}
	if q.mask&fName != 0 {
		v = append(v, noCopyBytes(q.name))
	}
	if q.mask&fAge != 0 {
		v = append(v, putI32(&b.age, q.age))
	}
	if q.mask&fStatus != 0 {
		v = append(v, noCopyBytes(q.status))
	}
	if q.mask&fSince != 0 {
		v = append(v, putTS(&b.since, q.since))
	}
	v = append(v, putI64(&b.limit, int64(q.limit)))
	b.vals = v
	return v
}

// FastExec is the pgconn-level executor.
type FastExec struct{ Pool *pgxpool.Pool }

// connStmts tracks which shapes this physical connection has PREPAREd. A
// connection is checked out by one goroutine at a time, so no lock is needed.
type connStmts struct {
	done    [nShapes]bool
	getDone bool
}

var stmtState sync.Map // *pgconn.PgConn -> *connStmts

// ForgetConn drops per-connection prepared-statement state. Wire this to
// pgxpool's BeforeClose so dead connections do not leak entries.
func ForgetConn(pc *pgconn.PgConn) { stmtState.Delete(pc) }

func stmtName(mask uint32) string { return "storm_u_" + strconv.FormatUint(uint64(mask), 10) }

// AllFast runs the query through pgconn: no []any, no type map, no Scan.
func (q Query) AllFast(ctx context.Context, ex FastExec, dst []Row, sl *Slab) ([]Row, error) {
	c, err := ex.Pool.Acquire(ctx)
	if err != nil {
		return dst, err
	}
	defer c.Release()
	pc := c.Conn().PgConn()

	st := lookup(q.mask)
	name := stmtName(q.mask)

	sv, _ := stmtState.LoadOrStore(pc, &connStmts{})
	cs := sv.(*connStmts)
	if !cs.done[q.mask] {
		if _, err := pc.Prepare(ctx, name, st.sql, nil); err != nil {
			return dst, err
		}
		cs.done[q.mask] = true
	}

	b := pbufs.Get().(*pbuf)
	params := q.encode(b)

	rr := pc.ExecPrepared(ctx, name, params, binFormats[:len(params)], binFormats[:8])
	for rr.NextRow() {
		dst = append(dst, Row{})
		scanRow(rr.Values(), &dst[len(dst)-1], sl)
	}
	_, err = rr.Close()
	pbufs.Put(b)
	return dst, err
}

// GetFast is the primary-key lookup over pgconn: one prepared statement, one
// 16-byte parameter, no []any anywhere.
func GetFast(ctx context.Context, ex FastExec, id [16]byte, dst *Row, sl *Slab) (bool, error) {
	c, err := ex.Pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer c.Release()
	pc := c.Conn().PgConn()

	sv, _ := stmtState.LoadOrStore(pc, &connStmts{})
	cs := sv.(*connStmts)
	if !cs.getDone {
		if _, err := pc.Prepare(ctx, "storm_u_get", getSQL, nil); err != nil {
			return false, err
		}
		cs.getDone = true
	}

	b := pbufs.Get().(*pbuf)
	b.org = id
	v := append(b.vals[:0], b.org[:])
	b.vals = v

	found := false
	rr := pc.ExecPrepared(ctx, "storm_u_get", v, binFormats[:1], binFormats[:8])
	for rr.NextRow() {
		scanRow(rr.Values(), dst, sl)
		found = true
	}
	_, err = rr.Close()
	pbufs.Put(b)
	return found, err
}
