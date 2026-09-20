// Package msdrv is storm's SQL Server adapter: a TDS client, written rather
// than wrapped.
//
// The measurement that decided it is in internal/mssqlspike/README.md.
// microsoft/go-mssqldb costs 11.3 allocations per row through the most
// favourable path it offers, against runtime/mydrv's 1.07 — because every
// column is boxed into a driver.Value before storm can see a byte, and there is
// no RawValues to ask for. That is ADR-0007's argument with more force than
// MySQL gave it.
//
// Stdlib only, like every other package under runtime/.
//
// TDS is a token stream inside a framed packet protocol, which is two layers
// where MySQL has one:
//
//   - PACKETS carry an 8-byte header and are capped at a negotiated size, so a
//     single logical message is split across several. This file is that layer:
//     a reader that hides the split from everything above it, and a writer that
//     fragments on the way out.
//   - TOKENS are the message. A result set is COLMETADATA followed by ROW
//     tokens followed by DONE, and they may straddle packet boundaries at any
//     byte — which is why the reader below is a byte stream and not a
//     packet-at-a-time API. Getting that wrong produces a client that works
//     until a result exceeds 4 KB.
package msdrv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// Packet types.
const (
	pktSQLBatch  = 1
	pktRPC       = 3
	pktReply     = 4
	pktAttention = 6
	pktBulkLoad  = 7
	pktTransMgr  = 14
	pktLogin7    = 16
	pktPrelogin  = 18
)

// Packet status flags.
const (
	statusNormal   = 0x00
	statusEOM      = 0x01
	statusIgnore   = 0x02
	statusResetCon = 0x08
)

// headerSize is the fixed packet header: type, status, length, spid, id, window.
const headerSize = 8

// defaultPacketSize is what the client asks for in LOGIN7.
//
// 4096 is the protocol's default and what the server assumes until told
// otherwise. Larger is negotiable and the server may cap it; the reader below
// handles whatever it settles on, because it reads the length out of every
// header rather than assuming one.
const defaultPacketSize = 4096

// conn is the framed connection: a byte stream over TDS packets.
//
// One buffer for reading and one for writing, both reused for the life of the
// connection. Nothing here allocates per packet, which is the property the
// whole adapter's allocation budget rests on.
type conn struct {
	c net.Conn

	// rbuf holds one inbound packet, header included. rpos and rend bracket the
	// payload not yet consumed.
	rbuf []byte
	rpos int
	rend int
	// rlast is the EOM flag of the packet in rbuf: when it is set and the
	// payload is spent, the message is over and reading further is an error
	// rather than a blocking read for a packet the server will not send.
	rlast bool
	// inMsg says a message is being read. Cleared when the EOM packet's payload
	// runs out.
	inMsg bool

	// wbuf holds one outbound packet. wpos is where the next byte goes.
	wbuf []byte
	wpos int
	// wtype is the packet type of the message being written.
	wtype byte
	// pktID is the sequence number, which wraps at 256. The server does not
	// check it, but a capture read by a human is unreadable without it.
	pktID byte

	// size is the negotiated packet size, header included.
	size int
	// pending is a packet size the server confirmed mid-message. Applied
	// between messages: resizing while a packet is on the wire would leave the
	// reader holding a buffer the wrong size for it.
	pending int

	// txn is the CURRENT transaction's descriptor, which every request after a
	// BEGIN must echo in its headers.
	//
	// Not decoration and not optional: the server answers a request carrying
	// the wrong descriptor with "New request is not allowed to start because it
	// should come with valid transaction descriptor", and a client that always
	// sends zero works perfectly until the first BEGIN TRANSACTION and then
	// fails every statement after it.
	txn uint64

	// nread counts the payload bytes of this message consumed in EARLIER
	// packets, so consumed() is a message offset rather than a packet one. A
	// token bounded by its declared length needs the former; every multi-packet
	// result would otherwise be bounded against the wrong origin.
	nread int
}

func newConn(c net.Conn) *conn {
	x := &conn{c: c, size: defaultPacketSize}
	x.rbuf = make([]byte, x.size)
	x.wbuf = make([]byte, x.size)
	return x
}

// resize adopts the packet size the server confirmed in its ENVCHANGE.
//
// Only ever called between messages — a resize mid-message would leave the
// reader holding a buffer the wrong size for a packet already on the wire.
func (x *conn) resize(n int) {
	if n < 512 || n == x.size {
		return
	}
	x.size = n
	x.rbuf = make([]byte, n)
	x.wbuf = make([]byte, n)
}

// ---- writing ---------------------------------------------------------------

// begin starts a message of the given type.
//
// A packet size the server confirmed mid-reply is applied here, which is the
// only safe moment: between messages, with nothing in flight on either buffer.
func (x *conn) begin(t byte) {
	if x.pending != 0 {
		x.resize(x.pending)
		x.pending = 0
	}
	x.wtype = t
	x.wpos = headerSize
}

// write appends to the message, flushing full packets as it fills them.
func (x *conn) write(p []byte) error {
	for len(p) > 0 {
		n := copy(x.wbuf[x.wpos:], p)
		x.wpos += n
		p = p[n:]
		if x.wpos == len(x.wbuf) {
			if err := x.flush(statusNormal); err != nil {
				return err
			}
		}
	}
	return nil
}

func (x *conn) writeByte(b byte) error {
	if x.wpos == len(x.wbuf) {
		if err := x.flush(statusNormal); err != nil {
			return err
		}
	}
	x.wbuf[x.wpos] = b
	x.wpos++
	return nil
}

func (x *conn) writeU16(v uint16) error {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	return x.write(b[:])
}

func (x *conn) writeU32(v uint32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return x.write(b[:])
}

func (x *conn) writeU64(v uint64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	return x.write(b[:])
}

// writeUCS2 writes a Go string as UTF-16LE, which is what every string in this
// protocol is.
func (x *conn) writeUCS2(s string) error {
	for _, r := range s {
		if r > 0xFFFF {
			r -= 0x10000
			if err := x.writeU16(uint16(0xD800 + (r >> 10))); err != nil {
				return err
			}
			if err := x.writeU16(uint16(0xDC00 + (r & 0x3FF))); err != nil {
				return err
			}
			continue
		}
		if err := x.writeU16(uint16(r)); err != nil {
			return err
		}
	}
	return nil
}

// flush writes the buffered packet with the given status.
func (x *conn) flush(status byte) error {
	n := x.wpos
	x.wbuf[0] = x.wtype
	x.wbuf[1] = status
	binary.BigEndian.PutUint16(x.wbuf[2:], uint16(n))
	binary.BigEndian.PutUint16(x.wbuf[4:], 0) // spid, client side is always 0
	x.wbuf[6] = x.pktID
	x.wbuf[7] = 0
	x.pktID++
	if _, err := x.c.Write(x.wbuf[:n]); err != nil {
		return err
	}
	x.wpos = headerSize
	return nil
}

// end finishes the message, marking the last packet.
func (x *conn) end() error {
	x.pktID = 0
	return x.flush(statusEOM)
}

// ---- reading ---------------------------------------------------------------

// errShortPacket is a packet whose header claims less than a header.
var errShortPacket = errors.New("msdrv: the server sent a packet shorter than its own header")

// errEndOfMessage is returned when a reader asks for bytes past the last packet
// of a message. It is a protocol error, never a normal end: every token knows
// its own length, so a reader that runs out has misparsed something upstream.
var errEndOfMessage = errors.New("msdrv: the token stream asked for bytes past the end of the message")

// next reads one packet into rbuf.
func (x *conn) next() error {
	if _, err := io.ReadFull(x.c, x.rbuf[:headerSize]); err != nil {
		return err
	}
	n := int(binary.BigEndian.Uint16(x.rbuf[2:]))
	if n < headerSize {
		return errShortPacket
	}
	if n > len(x.rbuf) {
		// The server is allowed to send a packet larger than the size it
		// confirmed — the negotiated size bounds what the CLIENT sends. Grow
		// rather than refuse, and keep the larger buffer, because if it
		// happened once it will happen again.
		nb := make([]byte, n)
		copy(nb, x.rbuf[:headerSize])
		x.rbuf = nb
	}
	if _, err := io.ReadFull(x.c, x.rbuf[headerSize:n]); err != nil {
		return err
	}
	if x.inMsg {
		x.nread += x.rend - headerSize
	} else {
		x.nread = 0
	}
	x.rlast = x.rbuf[1]&statusEOM != 0
	x.rpos = headerSize
	x.rend = n
	x.inMsg = true
	return nil
}

// fill makes at least one byte available, pulling the next packet if the
// current one is spent.
//
// The distinction that matters is between "this message has more packets" and
// "there is no message". Both look like an empty buffer, and reading rlast
// without checking inMsg first conflates them: the flag is left set by the LAST
// packet of the PREVIOUS message, so the first read of every new reply reports
// the end of one that already finished. The login then succeeds without reading
// the server's answer, and every statement after it parses the wrong stream.
func (x *conn) fill() error {
	for x.rpos >= x.rend {
		if x.inMsg {
			if x.rlast {
				x.inMsg = false
				return errEndOfMessage
			}
		}
		if err := x.next(); err != nil {
			return err
		}
	}
	return nil
}

// readByte reads one byte of the message.
func (x *conn) readByte() (byte, error) {
	if err := x.fill(); err != nil {
		return 0, err
	}
	b := x.rbuf[x.rpos]
	x.rpos++
	return b, nil
}

// readFull reads exactly len(p) bytes, across packet boundaries.
func (x *conn) readFull(p []byte) error {
	for len(p) > 0 {
		if err := x.fill(); err != nil {
			return err
		}
		n := copy(p, x.rbuf[x.rpos:x.rend])
		x.rpos += n
		p = p[n:]
	}
	return nil
}

// slice returns n bytes of the message WITHOUT copying when they are contiguous
// in the packet buffer, which is the whole point: a row's column values are
// handed to the scanner as sub-slices of the packet, so exposing a row costs
// nothing.
//
// When the value straddles a packet boundary it cannot be a sub-slice, so it is
// assembled into scratch — a caller-owned buffer reused across rows, so the
// copy costs no allocation either. That case is rare and unavoidable: the
// server decides where packets end.
func (x *conn) slice(n int, scratch *[]byte) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	if err := x.fill(); err != nil {
		return nil, err
	}
	if x.rend-x.rpos >= n {
		b := x.rbuf[x.rpos : x.rpos+n : x.rpos+n]
		x.rpos += n
		return b, nil
	}
	if cap(*scratch) < n {
		*scratch = make([]byte, n)
	}
	b := (*scratch)[:n]
	if err := x.readFull(b); err != nil {
		return nil, err
	}
	return b, nil
}

// skip discards n bytes of the message.
func (x *conn) skip(n int) error {
	for n > 0 {
		if err := x.fill(); err != nil {
			return err
		}
		k := x.rend - x.rpos
		if k > n {
			k = n
		}
		x.rpos += k
		n -= k
	}
	return nil
}

func (x *conn) readU16() (uint16, error) {
	var b [2]byte
	if err := x.readFull(b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b[:]), nil
}

func (x *conn) readU32() (uint32, error) {
	var b [4]byte
	if err := x.readFull(b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

func (x *conn) readU64() (uint64, error) {
	var b [8]byte
	if err := x.readFull(b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

// readUCS2 reads n CHARACTERS — not bytes — as a Go string.
//
// Every length in this protocol that describes a string is a character count,
// and every one of those characters is two bytes. Reading it as a byte count is
// the classic TDS bug: it half-reads every message and then desynchronises the
// stream, which presents as a protocol error several tokens later.
func (x *conn) readUCS2(chars int) (string, error) {
	if chars == 0 {
		return "", nil
	}
	buf := make([]byte, chars*2)
	if err := x.readFull(buf); err != nil {
		return "", err
	}
	return ucs2ToString(buf), nil
}

// readBVarchar reads a string with a one-byte character count.
func (x *conn) readBVarchar() (string, error) {
	n, err := x.readByte()
	if err != nil {
		return "", err
	}
	return x.readUCS2(int(n))
}

// readUSVarchar reads a string with a two-byte character count.
func (x *conn) readUSVarchar() (string, error) {
	n, err := x.readU16()
	if err != nil {
		return "", err
	}
	return x.readUCS2(int(n))
}

// ucs2ToString decodes UTF-16LE, surrogate pairs included.
func ucs2ToString(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	out := make([]rune, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u := uint32(b[i]) | uint32(b[i+1])<<8
		if u >= 0xD800 && u < 0xDC00 && i+3 < len(b) {
			lo := uint32(b[i+2]) | uint32(b[i+3])<<8
			if lo >= 0xDC00 && lo < 0xE000 {
				out = append(out, rune(0x10000+((u-0xD800)<<10)+(lo-0xDC00)))
				i += 2
				continue
			}
		}
		out = append(out, rune(u))
	}
	return string(out)
}

// ucs2Len is how many bytes a string takes as UTF-16LE.
func ucs2Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 4
		} else {
			n += 2
		}
	}
	return n
}

// drain reads and discards the rest of the current message.
//
// Called when a caller abandons a result: the connection is only reusable once
// the server's reply is fully consumed, and the alternative — closing it — is a
// new TCP handshake and a new login for every cancelled query.
func (x *conn) drain() error {
	for x.inMsg {
		if err := x.fill(); err != nil {
			if errors.Is(err, errEndOfMessage) {
				return nil
			}
			return err
		}
		x.rpos = x.rend
	}
	return nil
}

func protoErr(format string, a ...any) error {
	return fmt.Errorf("msdrv: "+format, a...)
}
