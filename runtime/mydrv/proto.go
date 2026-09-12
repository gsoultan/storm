// Package mydrv is storm's MySQL and MariaDB adapter.
//
// It speaks the wire protocol directly rather than wrapping a database/sql
// driver, and that is not a preference. storm's port wants raw bytes per column
// (runtime.Rows.RawValues) so the generated scanners can decode without boxing;
// go-sql-driver hands back DECODED values — int64, []uint8 — in both protocols,
// so satisfying the port on top of it would mean re-encoding. Measured, 200
// rows x 8 columns: 8.07 allocations per row through go-sql-driver, 9.07
// through vitess, 1.07 here — the same with decoding as without, because
// decoding raw bytes allocates nothing. BenchmarkQuery200x8 measures this
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
//   - Unix sockets are not supported: Config.Addr is host:port.
package mydrv

import (
	"bufio"
	"encoding/binary"
	"net"
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

// readPacket reads one packet into c.pkt, REUSING the buffer. The returned
// slice is valid until the next call — the same contract pgx's RawValues has,
// and the reason storm has Slabs.
func (c *conn) readPacket() ([]byte, error) {
	var h [4]byte
	if _, err := ioReadFull(c.r, h[:]); err != nil {
		return nil, err
	}
	n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
	c.seq = h[3] + 1
	if cap(c.pkt) < n {
		c.pkt = make([]byte, n)
	}
	c.pkt = c.pkt[:n]
	if _, err := ioReadFull(c.r, c.pkt); err != nil {
		return nil, err
	}
	return c.pkt, nil
}

func (c *conn) writePacket(body []byte) error {
	h := []byte{byte(len(body)), byte(len(body) >> 8), byte(len(body) >> 16), c.seq}
	if _, err := c.c.Write(append(h, body...)); err != nil {
		return err
	}
	c.seq++
	return nil
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
	nCols, _, _ := lenEncInt(p)
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
		if p[0] == 0xfe && len(p) < 9 {
			return nil // EOF
		}
		if p[0] == 0xff {
			return parseError(p)
		}
		off := 0
		for i := 0; i < int(nCols); i++ {
			if p[off] == 0xfb { // NULL
				c.cols[i] = nil
				off++
				continue
			}
			n, w, _ := lenEncInt(p[off:])
			off += w
			c.cols[i] = p[off : off+int(n)]
			off += int(n)
		}
		if err := fn(c.cols); err != nil {
			return err
		}
	}
}

func lenEncInt(b []byte) (v uint64, width int, ok bool) {
	switch {
	case b[0] < 0xfb:
		return uint64(b[0]), 1, true
	case b[0] == 0xfc:
		return uint64(binary.LittleEndian.Uint16(b[1:])), 3, true
	case b[0] == 0xfd:
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16, 4, true
	case b[0] == 0xfe:
		return binary.LittleEndian.Uint64(b[1:]), 9, true
	}
	return 0, 1, false
}

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
