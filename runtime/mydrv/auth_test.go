package mydrv

// Handshake tests against a hand-written server.
//
// These need no container on purpose: the cases that matter are the ones a
// working database never produces — a server that does not offer TLS when TLS
// was required, and a server that asks for the password in a recoverable form
// on a plaintext socket. Both are what an attacker would arrange, so both are
// tested against a server that arranges them.

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeServer speaks just enough of the handshake to answer one client.
type fakeServer struct {
	ln     net.Listener
	caps   uint32
	plugin string
	// after is what the server sends after the login packet: an OK, or an
	// AuthMoreData asking for full authentication.
	after []byte
	// login is the client's login packet, captured for the test to inspect.
	login chan []byte
}

func newFakeServer(t *testing.T, caps uint32, plugin string, after []byte) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{ln: ln, caps: caps, plugin: plugin, after: after, login: make(chan []byte, 1)}
	t.Cleanup(func() { ln.Close() })
	go s.serve()
	return s
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

func (s *fakeServer) serve() {
	c, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(packet(0, s.greeting())); err != nil {
		return
	}
	p, err := readOne(c)
	if err != nil {
		return
	}
	select {
	case s.login <- p:
	default:
	}
	_, _ = c.Write(packet(2, s.after))
	// Read whatever the client says next so it sees a clean close rather than
	// a reset, then stop: these tests end at the client's decision.
	_, _ = readOne(c)
}

func (s *fakeServer) greeting() []byte {
	b := []byte{10}
	b = append(b, "8.0.0-fake"...)
	b = append(b, 0)
	b = binary.LittleEndian.AppendUint32(b, 7) // connection id
	salt := []byte("ABCDEFGH")
	b = append(b, salt...)
	b = append(b, 0) // filler
	b = binary.LittleEndian.AppendUint16(b, uint16(s.caps))
	b = append(b, 45)                          // charset
	b = binary.LittleEndian.AppendUint16(b, 2) // status
	b = binary.LittleEndian.AppendUint16(b, uint16(s.caps>>16))
	b = append(b, 8+13)                // total auth-data length
	b = append(b, make([]byte, 10)...) // reserved
	b = append(b, "IJKLMNOPQRST"...)   // salt part two
	b = append(b, 0)
	b = append(b, s.plugin...)
	b = append(b, 0)
	return b
}

func packet(seq byte, body []byte) []byte {
	h := []byte{byte(len(body)), byte(len(body) >> 8), byte(len(body) >> 16), seq}
	return append(h, body...)
}

func readOne(c net.Conn) ([]byte, error) {
	h := make([]byte, 4)
	if _, err := readFull(c, h); err != nil {
		return nil, err
	}
	n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
	b := make([]byte, n)
	_, err := readFull(c, b)
	return b, err
}

func readFull(c net.Conn, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := c.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

const baseCaps = capLongPassword | capLongFlag | capProtocol41 | capSecureConn | capPluginAuth

// TLSRequired must refuse a server with no TLS. If it connected anyway, the
// mode would be a comment.
func TestTLSRequiredRefusesAPlaintextServer(t *testing.T) {
	s := newFakeServer(t, baseCaps, "mysql_native_password", []byte{0x00})
	_, err := Open(context.Background(), Config{
		Addr: s.addr(), User: "u", Password: "p", TLS: TLSRequired,
	})
	if !errors.Is(err, ErrTLSUnsupported) {
		t.Fatalf("err = %v, want ErrTLSUnsupported", err)
	}
}

// TLSPreferred is the default, and against a server with no TLS it must
// connect. This is the case that makes TLSPreferred not a security promise, and
// the reason TLSRequired exists.
func TestTLSPreferredConnectsToAPlaintextServer(t *testing.T) {
	s := newFakeServer(t, baseCaps, "mysql_native_password", []byte{0x00})
	c, err := Open(context.Background(), Config{Addr: s.addr(), User: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case login := <-s.login:
		if got := binary.LittleEndian.Uint32(login); got&capSSL != 0 {
			t.Error("the login packet asked for SSL against a server that does not offer it")
		}
	default:
		t.Fatal("the server never saw a login packet")
	}
}

// The full-auth exchange sends the password in a form the wire reveals. On a
// plaintext socket that must be a refusal, not a fallback.
func TestFullAuthIsRefusedWithoutTLS(t *testing.T) {
	s := newFakeServer(t, baseCaps, "caching_sha2_password", []byte{0x01, 0x04})
	_, err := Open(context.Background(), Config{
		Addr: s.addr(), User: "u", Password: "p", TLS: TLSDisabled,
	})
	if !errors.Is(err, ErrCleartextRefused) {
		t.Fatalf("err = %v, want ErrCleartextRefused", err)
	}
}

// ...and the refusal has to be about the ABSENCE of TLS, not about the plugin:
// with the caller's written consent the same exchange proceeds.
func TestFullAuthProceedsWhenAllowed(t *testing.T) {
	s := newFakeServer(t, baseCaps, "caching_sha2_password", []byte{0x01, 0x04})
	_, err := Open(context.Background(), Config{
		Addr: s.addr(), User: "u", Password: "p", TLS: TLSDisabled,
		AllowCleartextPasswordOverPlaintext: true,
	})
	// The fake server stops after the request for its public key, so the
	// connection fails — but NOT with the refusal, which is the point.
	if errors.Is(err, ErrCleartextRefused) {
		t.Fatal("refused even though the caller allowed it")
	}
}

// A server may name a plugin this driver does not speak. That has to be an
// error naming the plugin, not a hang or a silent empty password.
func TestUnknownAuthPluginIsNamed(t *testing.T) {
	sw := append([]byte{0xfe}, "sha256_password"...)
	sw = append(sw, 0)
	sw = append(sw, "SALTSALTSALTSALTSALT"...)
	sw = append(sw, 0)
	s := newFakeServer(t, baseCaps, "mysql_native_password", sw)
	_, err := Open(context.Background(), Config{Addr: s.addr(), User: "u", Password: "p"})
	if err == nil || !strings.Contains(err.Error(), "sha256_password") {
		t.Fatalf("err = %v, want one naming sha256_password", err)
	}
}

// The scramble must depend on the salt. A driver that sent a constant would
// authenticate against a replayed capture.
func TestNativePasswordDependsOnTheSalt(t *testing.T) {
	a := nativePassword("hunter2", []byte("ABCDEFGHIJKLMNOPQRST"))
	b := nativePassword("hunter2", []byte("TSRQPONMLKJIHGFEDCBA"))
	if len(a) != 20 {
		t.Fatalf("scramble is %d bytes, want 20", len(a))
	}
	if string(a) == string(b) {
		t.Error("the same scramble for two salts")
	}
	if nativePassword("", []byte("ABCDEFGHIJKLMNOPQRST")) != nil {
		t.Error("an empty password must send an empty scramble")
	}
}

func TestSHA256PasswordDependsOnTheSalt(t *testing.T) {
	a := sha256Password("hunter2", []byte("ABCDEFGHIJKLMNOPQRST"))
	b := sha256Password("hunter2", []byte("TSRQPONMLKJIHGFEDCBA"))
	if len(a) != 32 {
		t.Fatalf("scramble is %d bytes, want 32", len(a))
	}
	if string(a) == string(b) {
		t.Error("the same scramble for two salts")
	}
	if sha256Password("", []byte("ABCDEFGHIJKLMNOPQRST")) != nil {
		t.Error("an empty password must send an empty scramble")
	}
}

// The greeting's salt arrives in two pieces and the second is easy to drop.
// A 8-byte salt still authenticates against nothing, so it fails late.
func TestGreetingSaltIsBothParts(t *testing.T) {
	s := &fakeServer{caps: baseCaps, plugin: "caching_sha2_password"}
	g, err := parseGreeting(s.greeting())
	if err != nil {
		t.Fatal(err)
	}
	if string(g.salt) != "ABCDEFGHIJKLMNOPQRST" {
		t.Errorf("salt = %q, want both parts", g.salt)
	}
	if g.plugin != "caching_sha2_password" {
		t.Errorf("plugin = %q", g.plugin)
	}
	if g.connID != 7 {
		t.Errorf("connID = %d, want 7 — without it there is nothing to KILL QUERY", g.connID)
	}
}
