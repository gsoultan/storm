package msdrv

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Connecting: PRELOGIN, then optionally TLS, then LOGIN7.
//
// The middle step is where TDS is unlike anything else. Encryption is
// NEGOTIATED in PRELOGIN and then the TLS handshake itself runs INSIDE TDS
// packets — each handshake record wrapped in a packet of type 0x12 — and only
// afterwards does the connection become a plain TLS stream. Worse, the
// protocol's default is to encrypt the LOGIN PACKET ONLY: the handshake
// happens, the login goes over it, and then the connection reverts to
// plaintext for everything after.
//
// That is not an optimisation to skip. A server configured the default way will
// answer ENCRYPT_OFF, and a client that does not implement login-only
// encryption cannot log in to it at all.

// TLSMode says how far the client will go to protect the connection. The names
// mirror runtime/mydrv's, because a caller configuring two adapters should not
// have to learn two vocabularies.
type TLSMode int

const (
	// TLSPreferred is the protocol's own default: the LOGIN PACKET is
	// encrypted and the rest of the session is not. The certificate is not
	// verified, because the connection it protects lasts one packet.
	//
	// This is what a stock SQL Server offers, so it is the zero value — a
	// caller who configures nothing gets a login that is never sent in the
	// clear.
	TLSPreferred TLSMode = iota

	// TLSRequired encrypts the whole session and verifies the server's
	// certificate. Use it for anything crossing a network you do not own.
	TLSRequired

	// TLSInsecure encrypts the whole session and does NOT verify the
	// certificate. It stops a passive observer and not an active one; it is
	// here because a development SQL Server's certificate is self-signed and
	// the alternative is people reaching for TLSDisabled instead.
	TLSInsecure

	// TLSDisabled never encrypts, login included.
	//
	// The password then crosses the wire under an obfuscation that is a nibble
	// swap and an XOR — not encryption, and not meant to be. Only for a socket
	// that is already private, and the server has to agree: one configured with
	// ForceEncryption refuses, and this reports that rather than silently
	// upgrading to something the caller did not ask for.
	TLSDisabled
)

// Config is everything needed to reach a server.
type Config struct {
	// Addr is host:port. SQL Server's default port is 1433.
	Addr string

	User, Password, Database string

	TLS TLSMode
	// TLSConfig is used when upgrading, exactly as given. A nil value takes its
	// meaning from TLS: verified under TLSRequired, unverified otherwise.
	TLSConfig *tls.Config

	// AppName is what the server records as the application. It shows up in
	// sys.dm_exec_sessions, which is where a DBA looks first when a query is
	// slow, so it is worth setting to something a human recognises.
	AppName string

	// MaxConns caps how many connections a Pool opens. Zero means
	// DefaultMaxConns. Ignored by Open, which makes exactly one.
	MaxConns int

	// AcquireTimeout bounds how long a caller waits for a pooled connection.
	// Zero means DefaultAcquireTimeout; a negative value waits forever. See
	// runtime/mydrv's note on the same field: the alternative is a hang.
	AcquireTimeout time.Duration

	// PacketSize is what the client asks for in LOGIN7. Zero means 4096, the
	// protocol's default. The server may answer with something smaller.
	PacketSize int
}

// prelogin option tokens.
const (
	preVersion    = 0
	preEncryption = 1
	preInstOpt    = 2
	preThreadID   = 3
	preMARS       = 4
	preTerminator = 0xFF
)

// Encryption negotiation values.
const (
	encryptOff    = 0 // encrypt the login packet only
	encryptOn     = 1 // encrypt everything
	encryptNotSup = 2 // no encryption at all
	encryptReq    = 3 // the server demands encryption
)

// prelogin exchanges capabilities and returns the encryption the server chose.
func (x *conn) prelogin(want byte) (byte, error) {
	// Four options, written as a table so the offsets below are computed from
	// the same list the payload is.
	opts := []struct {
		token byte
		data  []byte
	}{
		// The version storm reports. Not read for anything but the server's
		// logs, and a zero here makes those logs useless.
		{preVersion, []byte{1, 0, 0, 0, 0, 0}},
		{preEncryption, []byte{want}},
		{preInstOpt, []byte{0}},
		{preThreadID, []byte{0, 0, 0, 0}},
		{preMARS, []byte{0}},
	}

	// Each header is 5 bytes: token, offset, length. Offsets are from the start
	// of the PRELOGIN payload, not the packet.
	off := len(opts)*5 + 1
	x.begin(pktPrelogin)
	for _, o := range opts {
		var h [5]byte
		h[0] = o.token
		binary.BigEndian.PutUint16(h[1:], uint16(off))
		binary.BigEndian.PutUint16(h[3:], uint16(len(o.data)))
		if err := x.write(h[:]); err != nil {
			return 0, err
		}
		off += len(o.data)
	}
	if err := x.writeByte(preTerminator); err != nil {
		return 0, err
	}
	for _, o := range opts {
		if err := x.write(o.data); err != nil {
			return 0, err
		}
	}
	if err := x.end(); err != nil {
		return 0, err
	}

	// The reply is the same shape. Read the whole payload and index into it,
	// because the option DATA is addressed by offset from the payload's start —
	// a streaming read cannot seek backwards.
	if err := x.next(); err != nil {
		return 0, err
	}
	body := x.rbuf[headerSize:x.rend]
	x.rpos = x.rend
	x.inMsg = false

	got := byte(encryptNotSup)
	for i := 0; i+1 <= len(body); {
		if body[i] == preTerminator {
			break
		}
		if i+5 > len(body) {
			return 0, protoErr("the server's PRELOGIN reply ends inside an option header")
		}
		token := body[i]
		o := int(binary.BigEndian.Uint16(body[i+1:]))
		n := int(binary.BigEndian.Uint16(body[i+3:]))
		if o+n > len(body) {
			return 0, protoErr("the server's PRELOGIN option %d points past the reply", token)
		}
		if token == preEncryption && n >= 1 {
			got = body[o]
		}
		i += 5
	}
	return got, nil
}

// handshakeConn wraps the TLS handshake in PRELOGIN packets.
//
// crypto/tls wants a net.Conn and will write handshake records to it directly.
// TDS requires those records to arrive inside packets of type 0x12, so this
// stands between them for the duration of the handshake and is discarded
// afterwards — at which point the tls.Conn talks to the raw socket and the
// framing above it resumes over the encrypted stream.
type handshakeConn struct {
	x *conn
	// rem is what is left of the current inbound packet's payload.
	rem []byte
}

func (h *handshakeConn) Write(p []byte) (int, error) {
	h.x.begin(pktPrelogin)
	if err := h.x.write(p); err != nil {
		return 0, err
	}
	if err := h.x.end(); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (h *handshakeConn) Read(p []byte) (int, error) {
	for len(h.rem) == 0 {
		if err := h.x.next(); err != nil {
			return 0, err
		}
		h.rem = h.x.rbuf[headerSize:h.x.rend]
		h.x.rpos = h.x.rend
		h.x.inMsg = false
	}
	n := copy(p, h.rem)
	h.rem = h.rem[n:]
	return n, nil
}

func (h *handshakeConn) Close() error                  { return h.x.c.Close() }
func (h *handshakeConn) LocalAddr() net.Addr           { return h.x.c.LocalAddr() }
func (h *handshakeConn) RemoteAddr() net.Addr          { return h.x.c.RemoteAddr() }
func (h *handshakeConn) SetDeadline(t time.Time) error { return h.x.c.SetDeadline(t) }
func (h *handshakeConn) SetReadDeadline(t time.Time) error {
	return h.x.c.SetReadDeadline(t)
}
func (h *handshakeConn) SetWriteDeadline(t time.Time) error {
	return h.x.c.SetWriteDeadline(t)
}

// ErrNoEncryption is returned when the caller asked for TLS and the server has
// none, or asked for none and the server requires it. Either way the two
// disagree about the security of the connection, and continuing would mean one
// of them is wrong about it.
var ErrNoEncryption = errors.New(
	"msdrv: the client and the server disagree about encryption — either the server has no " +
		"certificate configured and TLSRequired was asked for, or it has ForceEncryption set " +
		"and TLSDisabled was")

// upgrade runs the TLS handshake inside PRELOGIN packets and swaps the socket
// for the encrypted one.
func (x *conn) upgrade(cfg Config, host string) (*tls.Conn, error) {
	tc := cfg.TLSConfig
	if tc == nil {
		tc = &tls.Config{ServerName: host}
		if cfg.TLS != TLSRequired {
			tc.InsecureSkipVerify = true
		}
		// SQL Server 2016 and later speak TLS 1.2; Azure SQL requires it. There
		// is no reason to offer less.
		tc.MinVersion = tls.VersionTLS12
	}
	h := &handshakeConn{x: x}
	c := tls.Client(h, tc)
	if err := c.Handshake(); err != nil {
		return nil, handshakeError(err)
	}
	return c, nil
}

// ErrNegativeSerial is the self-signed certificate SQL Server generates for
// itself when none is configured — and which Go will not parse.
//
// Go 1.23 started rejecting certificates with a negative serial number, and the
// rejection happens while PARSING, before any verification, so
// InsecureSkipVerify does not reach it. SQL Server's auto-generated certificate
// frequently has one.
//
// This cannot be fixed from inside a library: the escape hatch is a GODEBUG
// setting, which belongs to the program. So it is REPORTED, with both ways out
// named, rather than swallowed into a generic handshake failure that sends the
// reader looking at their own configuration.
var ErrNegativeSerial = errors.New(
	"msdrv: the server's certificate has a negative serial number, which Go refuses to parse " +
		"(and InsecureSkipVerify does not help — the refusal happens before verification). " +
		"This is the certificate SQL Server generates for itself when none is configured.\n" +
		"       Either run with GODEBUG=x509negativeserial=1, give the server a real " +
		"certificate, or — on a socket that is already private — set TLS: msdrv.TLSDisabled.")

// handshakeError names the one failure that is not the caller's fault.
func handshakeError(err error) error {
	if strings.Contains(err.Error(), "negative serial number") {
		return fmt.Errorf("%w (%v)", ErrNegativeSerial, err)
	}
	return fmt.Errorf("msdrv: TLS handshake: %w", err)
}

// LOGIN7 option flags.
//
// These are BIT POSITIONS in two packed bytes, and the low bits of the first
// one are not options at all — they declare the client's byte order and
// character set. Setting a flag by its ordinal rather than its mask announces a
// big-endian EBCDIC client, and the server closes the connection without a
// message, which is a long way to look for a two-line mistake.
const (
	// fUseDB asks for a notification when the database changes.
	optUseDB = 0x20
	// fDatabase: an ERROR rather than a warning when the requested database is
	// not there. A connection that silently lands in master and runs the
	// schema's statements against it is the worst outcome available.
	optInitDBFatal = 0x40
	// fSetLang: likewise for the language.
	optSetLangFatal = 0x80

	// fODBC turns on the ODBC-compatible session defaults, which is what every
	// client uses and what the server's documentation assumes: ANSI nulls,
	// quoted identifiers, ANSI padding and warnings, and arithmetic abort.
	//
	// Quoted identifiers are the one that matters to storm, and it is on for a
	// belt-and-braces reason rather than a load-bearing one: compile/mssql
	// brackets every identifier precisely so the setting cannot decide whether
	// a name is a name or a string.
	optODBC = 0x02
)

// login7 authenticates.
func (x *conn) login7(cfg Config) error {
	host, _ := os.Hostname()
	if host == "" {
		host = "storm"
	}
	app := cfg.AppName
	if app == "" {
		app = "storm"
	}
	server := cfg.Addr
	if i := strings.LastIndex(server, ":"); i > 0 {
		server = server[:i]
	}

	// The fixed part is 94 bytes: the header fields, thirteen offset/length
	// pairs, the six-byte client id and the long SSPI count.
	const fixed = 94
	type field struct {
		s    string
		pw   bool
		off  int
		size int
	}
	fields := []*field{
		{s: host},
		{s: cfg.User},
		{s: cfg.Password, pw: true},
		{s: app},
		{s: server},
		{s: ""}, // extension / unused
		{s: "go-storm"},
		{s: ""}, // language
		{s: cfg.Database},
	}
	off := fixed
	for _, f := range fields {
		f.off = off
		f.size = ucs2Len(f.s)
		off += f.size
	}
	total := off

	x.begin(pktLogin7)
	w := func(err error) bool { return err != nil }
	if w(x.writeU32(uint32(total))) ||
		// TDS 7.4. Everything storm needs — DATETIME2, DATETIMEOFFSET, the MAX
		// types — arrived by 7.3, and 7.4 is what every server since 2012
		// speaks.
		w(x.writeU32(0x74000004)) ||
		w(x.writeU32(uint32(x.size))) ||
		w(x.writeU32(0x00000001)) || // client program version
		w(x.writeU32(uint32(os.Getpid()))) ||
		w(x.writeU32(0)) || // connection id
		w(x.writeByte(optUseDB|optInitDBFatal|optSetLangFatal)) ||
		w(x.writeByte(optODBC)) ||
		w(x.writeByte(0)) || // type flags
		w(x.writeByte(0)) || // option flags 3
		w(x.writeU32(0)) || // client time zone
		w(x.writeU32(0)) { // client LCID
		return protoErr("writing the login header")
	}
	for _, f := range fields {
		if err := x.writeU16(uint16(f.off)); err != nil {
			return err
		}
		// LENGTHS ARE IN CHARACTERS. Writing the byte count here is the classic
		// TDS mistake: the server reads twice as far as the field goes, and the
		// login fails with a message about the user name that has nothing to do
		// with the user name.
		if err := x.writeU16(uint16(f.size / 2)); err != nil {
			return err
		}
	}
	if err := x.write([]byte{0, 0, 0, 0, 0, 0}); err != nil { // client id
		return err
	}
	if w(x.writeU16(uint16(total))) || w(x.writeU16(0)) || // SSPI
		w(x.writeU16(uint16(total))) || w(x.writeU16(0)) || // attached db file
		w(x.writeU16(uint16(total))) || w(x.writeU16(0)) || // change password
		w(x.writeU32(0)) { // SSPI long
		return protoErr("writing the login offsets")
	}
	for _, f := range fields {
		if f.pw {
			if err := x.writePassword(f.s); err != nil {
				return err
			}
			continue
		}
		if err := x.writeUCS2(f.s); err != nil {
			return err
		}
	}
	return x.end()
}

// writePassword writes the password under TDS's obfuscation.
//
// Swap the nibbles of each UTF-16 byte and XOR with 0xA5. That is the whole
// algorithm, it is public, and it is reversible by anyone who reads the
// specification — which is precisely why TLSDisabled is documented the way it
// is. It is not a cipher; it is a courtesy against a shoulder-surfed packet
// capture.
func (x *conn) writePassword(s string) error {
	for _, r := range s {
		var u [2]uint16
		n := 1
		if r > 0xFFFF {
			r -= 0x10000
			u[0] = uint16(0xD800 + (r >> 10))
			u[1] = uint16(0xDC00 + (r & 0x3FF))
			n = 2
		} else {
			u[0] = uint16(r)
		}
		for i := 0; i < n; i++ {
			lo := byte(u[i])
			hi := byte(u[i] >> 8)
			if err := x.writeByte(((lo << 4) | (lo >> 4)) ^ 0xA5); err != nil {
				return err
			}
			if err := x.writeByte(((hi << 4) | (hi >> 4)) ^ 0xA5); err != nil {
				return err
			}
		}
	}
	return nil
}

// dial opens a connection and logs in.
func dial(ctx context.Context, cfg Config) (*conn, error) {
	d := net.Dialer{}
	nc, err := d.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	if t, ok := nc.(*net.TCPConn); ok {
		// Storm's whole point is small statements answered fast. Nagle batches
		// them into round trips that are not there to be saved.
		_ = t.SetNoDelay(true)
	}
	x := newConn(nc)
	if cfg.PacketSize > 0 {
		x.resize(cfg.PacketSize)
	}

	want := byte(encryptOff)
	switch cfg.TLS {
	case TLSRequired, TLSInsecure:
		want = encryptOn
	case TLSDisabled:
		want = encryptNotSup
	}

	got, err := x.prelogin(want)
	if err != nil {
		nc.Close()
		return nil, err
	}

	host := cfg.Addr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	var revert net.Conn

	switch {
	case got == encryptNotSup:
		if cfg.TLS == TLSRequired || cfg.TLS == TLSInsecure {
			nc.Close()
			return nil, ErrNoEncryption
		}
		// Nothing to upgrade; the login goes in the clear. Only reachable when
		// the caller asked for TLSDisabled, or when the server has no
		// certificate at all and the caller did not require one.
	case got == encryptReq && cfg.TLS == TLSDisabled:
		nc.Close()
		return nil, ErrNoEncryption
	default:
		tc, err := x.upgrade(cfg, host)
		if err != nil {
			nc.Close()
			return nil, err
		}
		x.c = tc
		x.pktID = 0
		if got == encryptOff {
			revert = nc
		}
	}

	if err := x.login7(cfg); err != nil {
		x.c.Close()
		return nil, err
	}
	if revert != nil {
		// Login-only encryption: the LOGIN7 PACKET is the last encrypted thing
		// on this connection. The server answers in plaintext, so the swap has
		// to happen after the write and BEFORE the read — reverting later would
		// hand the TLS layer a login acknowledgement that is not a TLS record,
		// and the connection would fail with a handshake error at the point of
		// success.
		x.c = revert
	}
	if err := x.readLogin(); err != nil {
		x.c.Close()
		return nil, err
	}
	return x, nil
}
