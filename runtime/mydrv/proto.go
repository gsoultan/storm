// Package mydrv is storm's MySQL and MariaDB adapter.
//
// It speaks the wire protocol directly rather than wrapping a database/sql
// driver, and that is not a preference. storm's port wants raw bytes per column
// (runtime.Rows.RawValues) so the generated scanners can decode without boxing;
// go-sql-driver hands back DECODED values — int64, []uint8 — in both protocols,
// so satisfying the port on top of it would mean re-encoding. Measured, 200
// rows x 8 columns: 8.07 allocations per row through go-sql-driver, 9.07
// through vitess, and ONE per 200-row query here — the same with decoding as
// without, because decoding raw bytes allocates nothing. (It was 1.07 per row
// until the packet header stopped escaping; see conn.hdr.) BenchmarkQuery200x8 measures this
// package; internal/mysqlspike/ has the comparison against the others, and
// TestQueryCostsAboutOneAllocationPerRow is the gate that keeps it true.
//
// No third-party dependency: this package is stdlib only, so an adopter who
// never targets MySQL links nothing extra and one who does links no driver
// either.
//
// # What it does and does not do
//
// Implemented: TLS (Config.TLS, verified by default), mysql_native_password and
// caching_sha2_password including the full-auth exchange, a bounded connection
// pool with transactions, and real cancellation — a cancelled context sends
// KILL QUERY from a second connection, so the statement stops on the SERVER and
// not just in this process.
//
// Known limits, stated here rather than in a release note:
//
//   - Result sets STREAM, so a query holds its CONNECTION until the rows are
//     closed. Callers must Close, which generated code does with a defer, and
//     a second statement on the same connection before then is ErrRowsOpen
//     rather than a garbled packet. Use a Pool if you nest.
//   - CopyFrom is emulated with a multi-row INSERT, because MySQL has no COPY.
//     See ErrNoCopyProtocol.
//   - Batch is N round trips, because MySQL's protocol has no pipeline. See
//     Conn.Batch.
//   - Bound parameters cover the types storm generates and not the whole MySQL
//     type table; an unbound type is an error, never a silent conversion.
//   - A pooled connection is waited for, not waited on forever:
//     Config.AcquireTimeout bounds it and ErrPoolExhausted names the usual
//     cause, which is a result set that was never closed.
package mydrv

import (
	"bufio"
	"encoding/binary"
	"errors"
	"net"
	"time"
)

type conn struct {
	c net.Conn
	// id is the server's thread id for this connection. A second connection
	// needs it to KILL QUERY the statement running on this one.
	id   uint32
	r    *bufio.Reader
	seq  uint8
	pkt  []byte   // reused packet buffer
	cols [][]byte // reused per-row column slices, pointing INTO pkt
	wbuf []byte   // reused outgoing frame: header plus one packet's payload

	// hdr is the incoming packet header. A field rather than a local because a
	// local array handed to the reader escapes, and that was one allocation
	// per PACKET — which is to say per row.
	hdr [4]byte

	// onOK, when set, is handed an OK packet so a caller can read the
	// affected-row count out of it. A field rather than a return value because
	// the OK packet arrives on a path shared with result sets.
	onOK func(p []byte)
}

// newConn wraps an already-dialled socket. Dialling is Open's job, so it can
// honour the caller's context.
func newConn(nc net.Conn) *conn {
	return &conn{c: nc, r: bufio.NewReaderSize(nc, 64<<10), pkt: make([]byte, 0, 64<<10)}
}

// upgrade swaps the socket for its TLS wrapper mid-handshake, and resets the
// reader — anything buffered from the plaintext side is not part of the tunnel.
func (c *conn) upgrade(tc net.Conn) {
	c.c = tc
	c.r = bufio.NewReaderSize(tc, 64<<10)
}

// maxPayload is the largest payload one wire packet carries. A payload of
// exactly this length means the logical packet CONTINUES in the next one; a
// payload of 16 MiB or more is a run of full packets ended by a shorter one,
// which may be empty.
const maxPayload = 1<<24 - 1

// maxLogicalPacket bounds what readPacket will assemble. It is the server's
// own ceiling — max_allowed_packet cannot be set above 1 GiB — so a server
// that claims more is corrupt or hostile, and growing a buffer to whatever it
// says is how a client hands it the process's memory. A variable so a test can
// reach it without a gigabyte.
var maxLogicalPacket = 1 << 30

// maxRetainedBuffer is the most a connection keeps between statements. The
// buffers are reused so that rows cost nothing, but a single 100 MiB row
// would otherwise pin 100 MiB to that connection for as long as the pool
// keeps it, which is every connection, eventually.
const maxRetainedBuffer = 1 << 20

// readPacket reads one LOGICAL packet into c.pkt, REUSING the buffer. The
// returned slice is valid until the next call — the same contract pgx's
// RawValues has, and the reason storm has Slabs.
//
// A value of 16 MiB or more arrives split across wire packets, and the pieces
// are reassembled here: every caller above this line reads a row, a column
// definition or an OK as one packet, and handing one of them the first 16 MiB
// of a row would read the rest of the row as the next packet.
func (c *conn) readPacket() ([]byte, error) {
	c.pkt = c.pkt[:0]
	for {
		if _, err := ioReadFull(c.r, c.hdr[:]); err != nil {
			return nil, err
		}
		n := int(c.hdr[0]) | int(c.hdr[1])<<8 | int(c.hdr[2])<<16
		c.seq = c.hdr[3] + 1
		off := len(c.pkt)
		if off+n > maxLogicalPacket {
			return nil, errPacketTooLarge
		}
		if cap(c.pkt)-off < n {
			grown := make([]byte, off, off+n)
			copy(grown, c.pkt)
			c.pkt = grown
		}
		c.pkt = c.pkt[:off+n]
		if _, err := ioReadFull(c.r, c.pkt[off:]); err != nil {
			return nil, err
		}
		if n < maxPayload {
			return c.pkt, nil
		}
	}
}

// errPacketTooLarge is a server claiming a packet past max_allowed_packet's
// own ceiling.
var errPacketTooLarge = errors.New("mydrv: the server sent a packet larger than MySQL allows")

// writePacket sends body as one logical packet, split into wire packets of at
// most maxPayload bytes.
//
// Splitting is not an edge case: a bound document or blob of 16 MiB used to
// go out with its length truncated to three bytes, and the server read the
// rest of it as garbage and dropped the connection. A body whose length is an
// exact multiple of maxPayload is followed by an empty packet, because a full
// one says "more follows".
//
// Each wire packet is framed in a reused buffer and written with ONE Write, so
// the header and payload share a syscall — and, under TLS, a record.
func (c *conn) writePacket(body []byte) error {
	for {
		n := min(len(body), maxPayload)
		c.wbuf = append(c.wbuf[:0], byte(n), byte(n>>8), byte(n>>16), c.seq)
		c.wbuf = append(c.wbuf, body[:n]...)
		if _, err := c.c.Write(c.wbuf); err != nil {
			return c.whyWriteFailed(err)
		}
		c.seq++
		body = body[n:]
		if n < maxPayload {
			return nil
		}
	}
}

// whyWriteFailed looks for the server's reason when a write is refused.
//
// A server that will not take a packet — one past max_allowed_packet is the
// common case — sends an ERR naming why and closes the connection, and the
// client still writing sees only "broken pipe". The reason is usually already
// in the receive buffer, so it is worth one short read: "packet bigger than
// max_allowed_packet" is actionable and a broken pipe is not.
func (c *conn) whyWriteFailed(err error) error {
	if c.c.SetReadDeadline(time.Now().Add(100*time.Millisecond)) != nil {
		return err
	}
	p, rerr := c.readPacket()
	if rerr == nil && len(p) > 0 && p[0] == 0xff {
		return parseError(p)
	}
	return err
}

// trim lets go of buffers a large statement grew past maxRetainedBuffer, so a
// connection's footprint between statements is bounded by the common case
// rather than by the largest value it ever carried.
func (c *conn) trim() {
	if cap(c.pkt) > maxRetainedBuffer {
		c.pkt = make([]byte, 0, 64<<10)
	}
	if cap(c.wbuf) > maxRetainedBuffer {
		c.wbuf = nil
	}
}

// query runs COM_QUERY and calls fn once per row with the column slices.
//
// The slices point INTO the packet buffer, which is reused: valid until the
// next row. That is exactly storm's Rows.RawValues contract.
func (c *conn) query(sql string, fn func(cols [][]byte) error) error {
	c.seq = 0
	body := make([]byte, 0, len(sql)+1)
	body = append(body, 3) // COM_QUERY
	body = append(body, sql...)
	if err := c.writePacket(body); err != nil {
		return err
	}
	p, err := c.readPacket()
	if err != nil {
		return err
	}
	// Every p[0] below is read from a packet the SERVER sized. A zero-length
	// packet is legal on the wire and would index past the end; a corrupt or
	// hostile server must get an error, not the process.
	if len(p) == 0 {
		return errShortPacket
	}
	if p[0] == 0xff {
		return parseError(p)
	}
	if p[0] == 0x00 || p[0] == 0xfe {
		// Only an OK packet carries an affected-row count. This driver does not
		// ask for CLIENT_DEPRECATE_EOF, so 0xfe here is an EOF, and reading a
		// count out of it would be reading whatever followed.
		if p[0] == 0x00 && c.onOK != nil {
			c.onOK(p)
		}
		return nil // OK or EOF: no result set
	}
	nCols, _, ok := lenEncInt(p)
	if !ok {
		return errShortPacket
	}
	for i := uint64(0); i < nCols; i++ {
		if _, err := c.readPacket(); err != nil { // column definitions, skipped
			return err
		}
	}
	// The EOF packet that closes the column definitions. Present because this
	// handshake does not set CLIENT_DEPRECATE_EOF — and not consuming it means
	// the first "row" read is the EOF, so the result looks empty. A real driver
	// sets the flag and skips this; the spike keeps it to stay closer to what
	// both servers do by default.
	if _, err := c.readPacket(); err != nil {
		return err
	}
	if cap(c.cols) < int(nCols) {
		c.cols = make([][]byte, nCols)
	}
	c.cols = c.cols[:nCols]

	for {
		p, err := c.readPacket()
		if err != nil {
			return err
		}
		if len(p) == 0 {
			return errShortPacket
		}
		if p[0] == 0xfe && len(p) < 9 {
			return nil // EOF
		}
		if p[0] == 0xff {
			return parseError(p)
		}
		// Bounds-checked the way the binary row reader is: a text row whose
		// columns do not fit inside its packet is errShortRow, not an index
		// past the buffer.
		off := 0
		for i := 0; i < int(nCols); i++ {
			if off >= len(p) {
				return errShortRow
			}
			if p[off] == 0xfb { // NULL
				c.cols[i] = nil
				off++
				continue
			}
			n, w, ok := lenEncInt(p[off:])
			if !ok || n > uint64(len(p)) || off+w+int(n) > len(p) {
				return errShortRow
			}
			off += w
			c.cols[i] = p[off : off+int(n)]
			off += int(n)
		}
		if err := fn(c.cols); err != nil {
			return err
		}
	}
}

// lenEncInt reads a length-encoded integer. ok is false for an unknown lead
// byte AND for a prefix that claims more bytes than b holds — the second case
// used to index past the slice, so a packet ending in 0xfc panicked the reader
// before the caller's own bounds check could run.
func lenEncInt(b []byte) (v uint64, width int, ok bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	switch {
	case b[0] < 0xfb:
		return uint64(b[0]), 1, true
	case b[0] == 0xfc:
		if len(b) < 3 {
			return 0, 0, false
		}
		return uint64(binary.LittleEndian.Uint16(b[1:])), 3, true
	case b[0] == 0xfd:
		if len(b) < 4 {
			return 0, 0, false
		}
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16, 4, true
	case b[0] == 0xfe:
		if len(b) < 9 {
			return 0, 0, false
		}
		return binary.LittleEndian.Uint64(b[1:]), 9, true
	}
	return 0, 1, false
}

// errShortPacket is a packet too short to carry the header byte the protocol
// says comes first. Like errShortRow, an error rather than a panic: the server
// chose the length, and a client must not let that choice take the process.
var errShortPacket = errors.New("mydrv: the server sent a packet too short to read")

func ioReadFull(r *bufio.Reader, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := r.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
