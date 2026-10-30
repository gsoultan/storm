package mydrv

// The wire framing and the connection bookkeeping, checked without a server:
// what goes onto a socket, what comes off one, and what a failure leaves
// behind. The live half of each of these is in harden_live_test.go.

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// Every width of length-encoded string, decoded back by the reader's own
// lenEncInt. Two of the four used to be written as the third, so a value of
// 64 KiB or more reached the server with its length taken modulo 65536.
func TestAppendLenEncRoundTripsEveryWidth(t *testing.T) {
	for _, n := range []int{0, 250, 251, 65535, 65536, 70000, 1<<24 - 1, 1 << 24} {
		v := bytes.Repeat([]byte{'x'}, n)
		b := appendLenEnc(nil, v)
		got, w, ok := lenEncInt(b)
		if !ok || got != uint64(n) {
			t.Fatalf("n=%d: decoded length %d (ok=%v)", n, got, ok)
		}
		if len(b) != w+n {
			t.Fatalf("n=%d: %d bytes written, want prefix %d + %d", n, len(b), w, n)
		}
	}
}

// A body of 16 MiB or more goes out as full packets ended by a shorter one —
// an EMPTY one when the length is an exact multiple — with consecutive
// sequence numbers; and the reader puts the pieces back together.
func TestPacketsOf16MiBAndMoreAreSplitAndReassembled(t *testing.T) {
	for _, n := range []int{0, 10, maxPayload - 1, maxPayload, maxPayload + 1, 2*maxPayload + 7} {
		body := make([]byte, n)
		for i := range body {
			body[i] = byte(i * 7)
		}
		var wire bytes.Buffer
		w := &conn{c: writeOnly{&wire}}
		if err := w.writePacket(body); err != nil {
			t.Fatal(err)
		}
		// The framing itself: lengths and sequence numbers, packet by packet.
		raw := wire.Bytes()
		frames, seq := 0, byte(0)
		for off := 0; off < len(raw); frames++ {
			l := int(raw[off]) | int(raw[off+1])<<8 | int(raw[off+2])<<16
			if raw[off+3] != seq {
				t.Fatalf("n=%d: frame %d has sequence %d, want %d", n, frames, raw[off+3], seq)
			}
			seq++
			off += 4 + l
			if l < maxPayload && off != len(raw) {
				t.Fatalf("n=%d: a short frame is followed by more bytes", n)
			}
		}
		if want := n/maxPayload + 1; frames != want {
			t.Fatalf("n=%d: %d frames, want %d", n, frames, want)
		}
		r := newConn(readOnly{bytes.NewReader(raw)})
		got, err := r.readPacket()
		if err != nil {
			t.Fatalf("n=%d: read back: %v", n, err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("n=%d: reassembled %d bytes that differ from the %d written", n, len(got), n)
		}
	}
}

// A server claiming a continuation past MySQL's own ceiling is refused before
// the reader grows a buffer to its say-so.
func TestAPacketPastTheCeilingIsRefused(t *testing.T) {
	old := maxLogicalPacket
	maxLogicalPacket = 2*maxPayload + 1
	defer func() { maxLogicalPacket = old }()
	full := append([]byte{0xff, 0xff, 0xff, 0}, make([]byte, maxPayload)...)
	var raw []byte
	for i := 0; i < 3; i++ {
		full[3] = byte(i)
		raw = append(raw, full...)
	}
	r := newConn(readOnly{bytes.NewReader(raw)})
	if _, err := r.readPacket(); !errors.Is(err, errPacketTooLarge) {
		t.Fatalf("err = %v, want errPacketTooLarge", err)
	}
	if cap(r.pkt) > maxLogicalPacket {
		t.Fatalf("the reader grew a %d-byte buffer before refusing", cap(r.pkt))
	}
}

// A column definition that ends before its type byte is an error, not an
// index past the packet: every offset in it is the server's.
func TestColumnDefinitionCutShortIsAnError(t *testing.T) {
	def := []byte{3, 'd', 'e', 'f', 0, 0, 0, 1, 'c', 0, 0x0c, 0x21, 0, 0xff, 0, 0, 0, typeLong, 0, 0}
	if typ, ok := columnType(def); !ok || typ != typeLong {
		t.Fatalf("a whole definition read as (%#x, %v), want (%#x, true)", typ, ok, typeLong)
	}
	// The type byte is at offset 17; every shorter cut ends before it.
	for n := 0; n <= 17; n++ {
		if _, ok := columnType(def[:n]); ok {
			t.Fatalf("a definition cut to %d bytes was accepted", n)
		}
	}
	if _, ok := columnType([]byte{0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}); ok {
		t.Fatal("a string claiming more bytes than the packet holds was accepted")
	}
}

// What settle keeps and what it throws away. A pool hands out whatever is not
// broken, so the mistake in either direction is expensive: keeping a dead
// socket fails every statement after a failover, and discarding a healthy one
// costs a reconnect per duplicate key.
func TestOnlyFailuresThatLeaveTheSocketUnknownBreakTheConnection(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		broke bool
	}{
		{"server error", &Error{Code: erDupEntry}, false},
		{"rows open", ErrRowsOpen, false},
		{"unbindable argument", &unboundError{errors.New("no binding")}, false},
		{"eof", io.EOF, true},
		{"reset", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, true},
		{"short row", errShortRow, true},
		{"server shutting down", &Error{Code: erServerShutdown}, true},
		{"wait_timeout", &Error{Code: erInteractionTimeout}, true},
		{"packet too large", &Error{Code: erNetPacketTooLarge}, true},
		{"mariadb killed", &Error{Code: erConnectionKilled}, true},
	} {
		x := &Conn{}
		if got := x.settle(c.err); got != c.err {
			t.Errorf("%s: settle returned %v, want the error unchanged", c.name, got)
		}
		if x.broken != c.broke {
			t.Errorf("%s: broken = %v, want %v", c.name, x.broken, c.broke)
		}
	}
}

// A non-UTC time is bound as its UTC wall clock, because that is what mydec
// reads DATETIME back as. The value's own zone stored time.Now() from a
// process at UTC+7 seven hours ahead.
func TestATimeIsBoundInUTC(t *testing.T) {
	in := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.FixedZone("x", 7*3600))
	b, err := appendBind(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b[1:]), "2026-01-01 20:04:05.000006"; got != want {
		t.Fatalf("bound %q, want %q", got, want)
	}
	doc, _ := appendJSONList(nil, []time.Time{in})
	if got, want := string(doc), `["2026-01-01 20:04:05.000006"]`; got != want {
		t.Fatalf("a list bound %s, want %s", got, want)
	}
}

// Reading a row costs no allocation per packet. The header used to be a local
// array, which escaped through the reader, so every ROW allocated four bytes.
func TestReadingAPacketDoesNotAllocate(t *testing.T) {
	var raw bytes.Buffer
	for i := 0; i < 200; i++ {
		raw.Write([]byte{3, 0, 0, byte(i), 'a', 'b', 'c'})
	}
	src := raw.Bytes()
	rd := bytes.NewReader(src)
	var nc io.Reader = readOnly{rd}
	r := newConn(readOnly{rd})
	if n := testing.AllocsPerRun(20, func() {
		rd.Reset(src)
		r.r.Reset(nc)
		for i := 0; i < 200; i++ {
			if _, err := r.readPacket(); err != nil {
				t.Fatal(err)
			}
		}
	}); n != 0 {
		t.Fatalf("%.0f allocations reading 200 packets, want 0", n)
	}
}

// A buffer grown by one large value is let go between statements, so a
// connection's footprint is set by the common case.
func TestALargeValueDoesNotPinItsBuffer(t *testing.T) {
	c := newConn(readOnly{bytes.NewReader(nil)})
	c.pkt = make([]byte, 0, 8<<20)
	c.wbuf = make([]byte, 0, 8<<20)
	c.trim()
	if cap(c.pkt) > maxRetainedBuffer || cap(c.wbuf) > maxRetainedBuffer {
		t.Fatalf("after trim: pkt %d, wbuf %d bytes retained", cap(c.pkt), cap(c.wbuf))
	}
}

// A connection whose peer has gone is not handed out again, and one whose
// peer is merely quiet is. The probe is what makes a server restart cost no
// failed statement.
func TestTheIdleProbeTellsAQuietPeerFromAGoneOne(t *testing.T) {
	quiet, qs := net.Pipe()
	defer quiet.Close()
	defer qs.Close()
	if !(&Conn{c: newConn(quiet)}).alive() {
		t.Fatal("a connection whose peer is quiet was reported dead")
	}

	gone, gs := net.Pipe()
	defer gone.Close()
	gs.Close()
	if (&Conn{c: newConn(gone)}).alive() {
		t.Fatal("a connection whose peer closed was reported alive")
	}

	// MySQL 8 says why before it drops an idle connection. Bytes on an idle
	// connection are never a good sign.
	talk, ts := net.Pipe()
	defer talk.Close()
	defer ts.Close()
	go ts.Write([]byte{9, 0, 0, 0, 0xff, 0xbf, 0x0f, '#', 'H', 'Y', '0', '0', '0'})
	time.Sleep(10 * time.Millisecond)
	if (&Conn{c: newConn(talk)}).alive() {
		t.Fatal("a connection with an unsolicited packet waiting was reported alive")
	}
}

// Close and a release that overlaps it cannot leave a connection open in the
// idle set with nothing left to close it.
func TestAReleaseRacingCloseDoesNotLeakTheConnection(t *testing.T) {
	for i := 0; i < 200; i++ {
		p := &Pool{idle: make(chan *Conn, 1), sem: make(chan struct{}, 1)}
		p.sem <- struct{}{}
		client, server := net.Pipe()
		c := &Conn{c: newConn(client)}
		done := make(chan struct{})
		go func() { p.release(c); close(done) }()
		p.Close()
		<-done
		p.Close()
		if len(p.idle) != 0 {
			t.Fatal("a connection was left in the idle set of a closed pool")
		}
		// The connection was closed, by one side or the other.
		_ = server.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := server.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("the released connection is still open: %v", err)
		}
		server.Close()
	}
}

// writeOnly and readOnly give a bytes buffer the net.Conn a conn expects.
type writeOnly struct{ w io.Writer }

func (w writeOnly) Write(p []byte) (int, error)    { return w.w.Write(p) }
func (writeOnly) Read([]byte) (int, error)         { return 0, io.EOF }
func (writeOnly) Close() error                     { return nil }
func (writeOnly) LocalAddr() net.Addr              { return nil }
func (writeOnly) RemoteAddr() net.Addr             { return nil }
func (writeOnly) SetDeadline(time.Time) error      { return nil }
func (writeOnly) SetReadDeadline(time.Time) error  { return nil }
func (writeOnly) SetWriteDeadline(time.Time) error { return nil }

type readOnly struct{ r io.Reader }

func (r readOnly) Read(p []byte) (int, error)     { return r.r.Read(p) }
func (readOnly) Write(p []byte) (int, error)      { return len(p), nil }
func (readOnly) Close() error                     { return nil }
func (readOnly) LocalAddr() net.Addr              { return nil }
func (readOnly) RemoteAddr() net.Addr             { return nil }
func (readOnly) SetDeadline(time.Time) error      { return nil }
func (readOnly) SetReadDeadline(time.Time) error  { return nil }
func (readOnly) SetWriteDeadline(time.Time) error { return nil }
