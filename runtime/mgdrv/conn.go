package mgdrv

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"
)

// conn is one connection, speaking OP_MSG.
//
// It owns one logical session id for its whole life, and a transaction number
// that only grows: a transaction pins its connection, so connection and
// session can be one thing, and no session ever needs starting or ending.
type conn struct {
	nc  net.Conn
	br  *bufio.Reader
	db  string
	out []byte
	in  []byte
	req int32

	lsid   [16]byte
	txnNum int64

	maxMsg int
	broken bool
}

// minWire is MongoDB 6.0. Older servers lack nothing this client sends but
// are out of support, and testing against them is not something storm does.
const minWire = 17

const opMsg = 2013

func dial(ctx context.Context, cfg *Config) (*conn, error) {
	d := net.Dialer{Timeout: cfg.DialTimeout}
	nc, err := d.DialContext(ctx, "tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	if cfg.TLS != nil {
		tc := tls.Client(nc, cfg.TLS)
		if err := tc.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, err
		}
		nc = tc
	}
	c := &conn{nc: nc, br: bufio.NewReaderSize(nc, 32<<10), db: cfg.Database, maxMsg: 48 << 20}
	if _, err := rand.Read(c.lsid[:]); err != nil {
		nc.Close()
		return nil, err
	}
	c.lsid[6] = c.lsid[6]&0x0f | 0x40
	c.lsid[8] = c.lsid[8]&0x3f | 0x80
	if err := c.handshake(ctx, cfg); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

func (c *conn) close() { c.nc.Close() }

// handshake is hello, and then authentication when a user is configured.
func (c *conn) handshake(ctx context.Context, cfg *Config) error {
	reply, err := c.run(ctx, func(b []byte) []byte {
		b = appendInt32(b, "hello", 1)
		b = appendBool(b, "helloOk", true)
		b = appendName(b, tDocument, "client")
		b, cl := startDoc(b)
		b = appendName(b, tDocument, "driver")
		b, dr := startDoc(b)
		b = appendStr(b, "name", "storm-mgdrv")
		b = appendStr(b, "version", "1")
		b = endDoc(b, dr)
		b = appendName(b, tDocument, "os")
		b, os := startDoc(b)
		b = appendStr(b, "type", goruntime.GOOS)
		b = endDoc(b, os)
		if cfg.AppName != "" {
			b = appendName(b, tDocument, "application")
			b, ap := startDoc(b)
			b = appendStr(b, "name", cfg.AppName)
			b = endDoc(b, ap)
		}
		b = endDoc(b, cl)
		return appendStr(b, "$db", "admin")
	})
	if err != nil {
		return err
	}
	if w := intField(reply, "maxWireVersion"); w < minWire {
		return errors.New("mgdrv: the server's wire version is " + strconv.FormatInt(w, 10) +
			"; storm needs MongoDB 6.0 or later")
	}
	if t, v, ok := lookup(reply, "isWritablePrimary"); !ok || t != tBool || v[0] == 0 {
		if strField(reply, "msg") != "isdbgrid" {
			return errors.New("mgdrv: " + cfg.Addr + " is not a writable primary — this client " +
				"talks to the one server it is given, so give it the primary (or a mongos)")
		}
	}
	if n := intField(reply, "maxMessageSizeBytes"); n > 0 {
		c.maxMsg = int(n)
	}
	if cfg.User != "" {
		return c.scram(ctx, cfg)
	}
	return nil
}

// run sends one command built by build — which writes its elements, $db
// included — and returns the reply document if the server said ok.
//
// The reply is valid until the next call on this connection.
func (c *conn) run(ctx context.Context, build func([]byte) []byte) ([]byte, error) {
	return c.runErr(ctx, func(b []byte) ([]byte, error) { return build(b), nil })
}

// runErr is run for a builder that can fail — an argument with no BSON form.
// Nothing is sent then: a command that went out half-encoded could insert a
// document with fields missing.
func (c *conn) runErr(ctx context.Context, build func([]byte) ([]byte, error)) ([]byte, error) {
	b := c.out[:0]
	b = append(b, make([]byte, 21)...) // header, flags, section kind
	b, at := startDoc(b)
	b, err := build(b)
	if err != nil {
		return nil, err
	}
	b = endDoc(b, at)
	c.out = b
	reply, err := c.roundTrip(ctx, b)
	if err != nil {
		return nil, err
	}
	return reply, okOf(reply)
}

// roundTrip frames msg (whose first 21 bytes are reserved for the header) and
// reads the reply's body document.
func (c *conn) roundTrip(ctx context.Context, msg []byte) ([]byte, error) {
	if len(msg) > c.maxMsg {
		return nil, errors.New("mgdrv: command of " + strconv.Itoa(len(msg)) +
			" bytes exceeds the server's message limit")
	}
	c.req++
	binary.LittleEndian.PutUint32(msg[0:], uint32(len(msg)))
	binary.LittleEndian.PutUint32(msg[4:], uint32(c.req))
	binary.LittleEndian.PutUint32(msg[8:], 0)
	binary.LittleEndian.PutUint32(msg[12:], opMsg)
	binary.LittleEndian.PutUint32(msg[16:], 0)
	msg[20] = 0

	if dl, ok := ctx.Deadline(); ok {
		c.nc.SetDeadline(dl)
	} else {
		c.nc.SetDeadline(time.Time{})
	}
	// Cancellation interrupts the read by moving the deadline into the past.
	// The connection is then mid-reply and is discarded; the server finishes
	// or abandons the command on its own, which for a read is harmless and
	// for a write is the same uncertainty a dropped network gives.
	stop := context.AfterFunc(ctx, func() { c.nc.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	reply, err := c.exchange(msg)
	if err != nil {
		c.broken = true
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return reply, nil
}

func (c *conn) exchange(msg []byte) ([]byte, error) {
	if _, err := c.nc.Write(msg); err != nil {
		return nil, err
	}
	var h [16]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return nil, err
	}
	n := int(int32(binary.LittleEndian.Uint32(h[0:])))
	if binary.LittleEndian.Uint32(h[12:]) != opMsg || n < 21 || n > c.maxMsg+(1<<20) {
		return nil, errors.New("mgdrv: unexpected reply from the server")
	}
	if cap(c.in) < n-16 {
		c.in = make([]byte, n-16)
	}
	body := c.in[:n-16]
	if _, err := io.ReadFull(c.br, body); err != nil {
		return nil, err
	}
	flags := binary.LittleEndian.Uint32(body)
	if flags&1 != 0 { // checksumPresent: the CRC trails the sections
		body = body[:len(body)-4]
	}
	for i := 4; i < len(body); {
		kind := body[i]
		i++
		if i+4 > len(body) {
			break
		}
		size := int(int32(binary.LittleEndian.Uint32(body[i:])))
		if size < 5 || i+size > len(body) {
			break
		}
		if kind == 0 {
			return body[i : i+size], nil
		}
		i += size
	}
	return nil, errMalformed
}

// okOf reports the server's verdict on a reply.
func okOf(reply []byte) error {
	t, v, _ := lookup(reply, "ok")
	if n, _ := asInt64(t, v); n == 1 {
		return nil
	}
	e := &Error{
		Code:    int(intField(reply, "code")),
		Name:    strField(reply, "codeName"),
		Message: strField(reply, "errmsg"),
	}
	if t, v, ok := lookup(reply, "errorLabels"); ok && t == tArray {
		_ = elements(v, func(t byte, _ string, v []byte) bool {
			if s, ok := asString(t, v); ok {
				e.Labels = append(e.Labels, s)
			}
			return true
		})
	}
	return classify(e)
}

// writeError reports the first per-document failure of a write that the
// server otherwise answered ok — a duplicate key on an insert arrives this way.
func writeError(reply []byte) error {
	for _, key := range []string{"writeErrors", "writeConcernError"} {
		t, v, ok := lookup(reply, key)
		if !ok {
			continue
		}
		doc := v
		if t == tArray {
			doc = nil
			_ = elements(v, func(t byte, _ string, v []byte) bool {
				if t == tDocument {
					doc = v
				}
				return false
			})
		}
		if doc == nil {
			continue
		}
		return classify(&Error{
			Code:    int(intField(doc, "code")),
			Name:    strField(doc, "codeName"),
			Message: strField(doc, "errmsg"),
		})
	}
	return nil
}

// ---- SCRAM-SHA-256 ----

// scram authenticates with SCRAM-SHA-256 (RFC 7677), the mechanism every
// supported MongoDB offers by default.
//
// The password is used as given. SASLprep (RFC 4013) normalises non-ASCII
// passwords, and this client does not implement it, so a non-ASCII password
// is refused here by name rather than failing as a wrong password.
func (c *conn) scram(ctx context.Context, cfg *Config) error {
	for i := 0; i < len(cfg.Password); i++ {
		if cfg.Password[i] >= 0x80 {
			return errors.New("mgdrv: the password is not ASCII; SCRAM needs SASLprep for it, " +
				"which this client does not implement")
		}
	}
	src := cfg.AuthSource
	if src == "" {
		src = "admin"
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	nonce := base64.StdEncoding.EncodeToString(raw[:])
	user := strings.NewReplacer("=", "=3D", ",", "=2C").Replace(cfg.User)
	first := "n=" + user + ",r=" + nonce

	reply, err := c.run(ctx, func(b []byte) []byte {
		b = appendInt32(b, "saslStart", 1)
		b = appendStr(b, "mechanism", "SCRAM-SHA-256")
		b = appendBinary(b, "payload", 0, []byte("n,,"+first))
		b = appendInt32(b, "autoAuthorize", 1)
		b = appendName(b, tDocument, "options")
		b, o := startDoc(b)
		b = appendBool(b, "skipEmptyExchange", true)
		b = endDoc(b, o)
		return appendStr(b, "$db", src)
	})
	if err != nil {
		return err
	}
	conv := intField(reply, "conversationId")
	serverFirst := payloadOf(reply)
	attrs := scramAttrs(serverFirst)
	if !strings.HasPrefix(attrs["r"], nonce) {
		return errors.New("mgdrv: SCRAM: the server's nonce does not extend ours")
	}
	salt, err := base64.StdEncoding.DecodeString(attrs["s"])
	if err != nil {
		return errors.New("mgdrv: SCRAM: bad salt")
	}
	iter, err := strconv.Atoi(attrs["i"])
	if err != nil || iter < 4096 {
		return errors.New("mgdrv: SCRAM: iteration count below 4096")
	}
	salted, err := pbkdf2.Key(sha256.New, cfg.Password, salt, iter, sha256.Size)
	if err != nil {
		return err
	}
	clientKey := mac(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	final := "c=biws,r=" + attrs["r"]
	authMsg := first + "," + serverFirst + "," + final
	sig := mac(stored[:], authMsg)
	proof := make([]byte, len(clientKey))
	for i := range proof {
		proof[i] = clientKey[i] ^ sig[i]
	}
	final += ",p=" + base64.StdEncoding.EncodeToString(proof)

	reply, err = c.run(ctx, func(b []byte) []byte {
		b = appendInt32(b, "saslContinue", 1)
		b = appendInt32(b, "conversationId", int32(conv))
		b = appendBinary(b, "payload", 0, []byte(final))
		return appendStr(b, "$db", src)
	})
	if err != nil {
		return err
	}
	want := base64.StdEncoding.EncodeToString(mac(mac(salted, "Server Key"), authMsg))
	if scramAttrs(payloadOf(reply))["v"] != want {
		return errors.New("mgdrv: SCRAM: the server's signature does not verify — " +
			"this is not the server the credentials belong to")
	}
	for {
		t, v, _ := lookup(reply, "done")
		if t == tBool && v[0] == 1 {
			break
		}
		reply, err = c.run(ctx, func(b []byte) []byte {
			b = appendInt32(b, "saslContinue", 1)
			b = appendInt32(b, "conversationId", int32(conv))
			b = appendBinary(b, "payload", 0, nil)
			return appendStr(b, "$db", src)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func mac(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

func payloadOf(reply []byte) string {
	t, v, ok := lookup(reply, "payload")
	if !ok || t != tBinary || len(v) < 5 {
		return ""
	}
	return string(v[5:])
}

func scramAttrs(s string) map[string]string {
	m := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}
