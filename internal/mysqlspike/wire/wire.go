// A minimal MySQL/MariaDB wire client — enough to answer one question: can
// storm's port (raw bytes per column, zero-copy, row at a time) be satisfied at
// the allocation profile ADR-0007 demands?
//
// NOT a driver. No TLS, no caching_sha2_password, no prepared statements, no
// pooling, no cancellation. Those are the four weeks; this is the risk in them.
package main

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
}

func dial(addr, user, pass, db string) (*conn, error) {
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &conn{c: nc, r: bufio.NewReaderSize(nc, 64<<10), pkt: make([]byte, 0, 64<<10)}
	if err := c.handshake(user, pass, db); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
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
	i++      // server version NUL
	i += 4   // thread id
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

func main() {}
