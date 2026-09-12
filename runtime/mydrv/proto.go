// Package mydrv is storm's MySQL and MariaDB adapter.
//
// It speaks the wire protocol directly rather than wrapping a database/sql
// driver, and that is not a preference. storm's port wants raw bytes per column
// (runtime.Rows.RawValues) so the generated scanners can decode without boxing;
// go-sql-driver hands back DECODED values — int64, []uint8 — in both protocols,
// so satisfying the port on top of it would mean re-encoding. Measured, 200
// rows x 8 columns: 8.07 allocations per row through go-sql-driver, 9.07
// through vitess, 1.04 here with decoding included. See
// internal/mysqlspike/ for the measurements.
//
// No third-party dependency: this package is stdlib only, so an adopter who
// never targets MySQL links nothing extra and one who does links no driver
// either.
//
// # NOT YET PRODUCTION READY
//
// Stated here rather than in a release note, because the gap is the kind that
// bites in production and not in a test:
//
//   - **No TLS.** Every connection is plaintext. Do not point this at a
//     database across a network you do not own.
//   - **mysql_native_password only.** MySQL 8.4 turns that off by default, so
//     this connects to MariaDB and to a MySQL configured for it.
//     caching_sha2_password is not implemented.
//   - **No connection pooling.** One Conn is one connection, and it is not safe
//     for concurrent use.
//   - **Context cancellation is a deadline, not a kill.** Cancelling does not
//     send COM_KILL_QUERY, so a long statement runs to completion server-side.
//   - **Bound parameters cover the types storm generates** and not the whole
//     MySQL type table.
//
// Those are the remaining work, and each one is a reason not to ship this
// against a real database yet.
package mydrv

import (
	"bufio"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

type conn struct {
	c    net.Conn
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

func (c *conn) handshake(user, pass, db string) error {
	p, err := c.readPacket()
	if err != nil {
		return err
	}
	// Protocol 10: version string, thread id, then the 20-byte scramble in two
	// pieces — the classic layout mysql_native_password uses.
	i := 1
	for i < len(p) && p[i] != 0 {
		i++
	}
	i++    // server version NUL
	i += 4 // thread id
	salt := append([]byte{}, p[i:i+8]...)
	i += 8 + 1 + 2 + 1 + 2 + 2 + 1 + 10
	if i+12 <= len(p) {
		salt = append(salt, p[i:i+12]...)
	}

	const (
		clientLongPassword  = 1
		clientLongFlag      = 4
		clientConnectWithDB = 8
		clientProtocol41    = 512
		clientSecureConn    = 1 << 15
		clientPluginAuth    = 1 << 19
	)
	flags := uint32(clientLongPassword | clientLongFlag | clientProtocol41 | clientSecureConn | clientPluginAuth)
	if db != "" {
		flags |= clientConnectWithDB
	}

	auth := nativePassword(pass, salt)
	body := make([]byte, 0, 128)
	body = binary.LittleEndian.AppendUint32(body, flags)
	body = binary.LittleEndian.AppendUint32(body, 64<<20) // max packet
	body = append(body, 45)                               // utf8mb4
	body = append(body, make([]byte, 23)...)
	body = append(body, user...)
	body = append(body, 0)
	body = append(body, byte(len(auth)))
	body = append(body, auth...)
	if db != "" {
		body = append(body, db...)
		body = append(body, 0)
	}
	body = append(body, "mysql_native_password"...)
	body = append(body, 0)
	if err := c.writePacket(body); err != nil {
		return err
	}
	p, err = c.readPacket()
	if err != nil {
		return err
	}
	if p[0] == 0xff {
		return fmt.Errorf("auth failed: %s", p[9:])
	}
	return nil
}

// nativePassword is SHA1(pass) XOR SHA1(salt + SHA1(SHA1(pass))).
func nativePassword(pass string, salt []byte) []byte {
	if pass == "" {
		return nil
	}
	h1 := sha1.Sum([]byte(pass))
	h2 := sha1.Sum(h1[:])
	h := sha1.New()
	h.Write(salt)
	h.Write(h2[:])
	out := h.Sum(nil)
	for i := range out {
		out[i] ^= h1[i]
	}
	return out
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
		return errors.New(string(p[9:]))
	}
	if p[0] == 0x00 || p[0] == 0xfe {
		return nil // OK packet: no result set
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
			return errors.New(string(p[9:]))
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
