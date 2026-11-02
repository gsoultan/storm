package redisdrv_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime/redisdrv"
)

// fake is a RESP server whose every reply is written by the test, so the
// replies a real server never sends — malformed, oversized, slow — can be.
type fake struct {
	ln    net.Listener
	dials atomic.Int64
}

func serve(t *testing.T, reply func(cmd []string) string) *fake {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			f.dials.Add(1)
			go func() {
				defer nc.Close()
				r := bufio.NewReader(nc)
				for {
					cmd, err := readCommand(r)
					if err != nil {
						return
					}
					out := reply(cmd)
					if out == "HANG" {
						io.Copy(io.Discard, r)
						return
					}
					if out == "CLOSE" {
						return
					}
					nc.Write([]byte(out))
				}
			}()
		}
	}()
	return f
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
	cmd := make([]string, n)
	for i := range cmd {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		l, _ := strconv.Atoi(strings.TrimSpace(hdr[1:]))
		buf := make([]byte, l+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		cmd[i] = string(buf[:l])
	}
	return cmd, nil
}

func (f *fake) client(o redisdrv.Options) *redisdrv.Client {
	o.Addr = f.ln.Addr().String()
	return redisdrv.Open(o)
}

func TestParseURL(t *testing.T) {
	for _, c := range []struct {
		url  string
		want redisdrv.Options
		tls  bool
	}{
		{"redis://", redisdrv.Options{Addr: "localhost:6379"}, false},
		{"redis://h:7000/2", redisdrv.Options{Addr: "h:7000", DB: 2}, false},
		{"redis://:pw@h", redisdrv.Options{Addr: "h:6379", Password: "pw"}, false},
		{"redis://u:pw@h/", redisdrv.Options{Addr: "h:6379", Username: "u", Password: "pw"}, false},
		{"rediss://[::1]:6380", redisdrv.Options{Addr: "[::1]:6380"}, true},
	} {
		got, err := redisdrv.ParseURL(c.url)
		if err != nil {
			t.Fatalf("%s: %v", c.url, err)
		}
		if (got.TLS != nil) != c.tls {
			t.Fatalf("%s: TLS %v", c.url, got.TLS != nil)
		}
		got.TLS = nil
		if got != c.want {
			t.Fatalf("%s: %+v, want %+v", c.url, got, c.want)
		}
	}
	for _, bad := range []string{"http://h", "redis://h/x", "redis://h/-1", "redis://%zz"} {
		if _, err := redisdrv.ParseURL(bad); err == nil {
			t.Fatalf("%s parsed", bad)
		}
		if _, err := redisdrv.OpenURL(bad); err == nil {
			t.Fatalf("%s opened", bad)
		}
	}
}

func TestMalformedRepliesDiscardTheConnection(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{
		"?what\r\n",     // unknown type
		"\r\n",          // empty line
		"+ok\n",         // no CR
		":nan\r\n",      // bad integer
		"$x\r\n",        // bad length
		"$-2\r\n",       // negative length
		"$3\r\nabcXY",   // no CRLF after the bulk
		"$99999999\r\n", // longer than MaxReplyBytes
		"*x\r\n",        // bad array length
		"*1\r\n?\r\n",   // bad element
		"$5\r\nab",      // cut short
		"*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n*1\r\n:1\r\n", // too deep
		"+" + strings.Repeat("x", 5000) + "\r\n",                       // a line longer than the buffer
	} {
		f := serve(t, func(cmd []string) string {
			if cmd[0] == "PING" {
				return "+PONG\r\n"
			}
			return raw
		})
		c := f.client(redisdrv.Options{MaxReplyBytes: 1 << 20, IOTimeout: 200 * time.Millisecond})
		if _, err := c.Do(ctx, "GET", "k"); err == nil {
			t.Fatalf("%q: no error", raw)
		}
		if err := c.Ping(ctx); err != nil {
			t.Fatalf("%q: the next command failed too: %v", raw, err)
		}
		if f.dials.Load() != 2 {
			t.Fatalf("%q: %d dials; a broken connection was reused", raw, f.dials.Load())
		}
		c.Close()
	}
}

func TestReplyShapes(t *testing.T) {
	ctx := context.Background()
	f := serve(t, func(cmd []string) string {
		switch cmd[0] {
		case "GET":
			if cmd[1] == "int" {
				return ":1\r\n"
			}
			if cmd[1] == "simple" {
				return "+hi\r\n"
			}
			return "*-1\r\n"
		case "MGET":
			if cmd[1] == "short" {
				return "*1\r\n$1\r\na\r\n"
			}
			if cmd[1] == "int" {
				return "*1\r\n:1\r\n"
			}
			if cmd[1] == "err" {
				return "-ERR no\r\n"
			}
			return ":5\r\n"
		case "DEL":
			return "+OK\r\n"
		}
		return "-ERR unknown\r\n"
	})
	c := f.client(redisdrv.Options{})
	defer c.Close()
	if _, err := c.Get(ctx, "int"); err == nil {
		t.Fatal("an integer read as a value")
	}
	if v, err := c.Get(ctx, "simple"); err != nil || string(v) != "hi" {
		t.Fatalf("simple string: %q %v", v, err)
	}
	if v, err := c.Get(ctx, "nilarray"); err != nil || v != nil {
		t.Fatalf("null array: %q %v", v, err)
	}
	for _, k := range [][]string{{"short", "b"}, {"int"}, {"err"}, {"notarray"}} {
		if _, err := c.MGet(ctx, k); err == nil {
			t.Fatalf("MGet %v: no error", k)
		}
	}
	if v, err := c.MGet(ctx, nil); v != nil || err != nil {
		t.Fatal("MGet of nothing")
	}
	if _, err := c.Del(ctx, "k"); err == nil {
		t.Fatal("DEL answered with a string")
	}
	if n, err := c.Del(ctx); n != 0 || err != nil {
		t.Fatal("Del of nothing")
	}
	if _, err := c.Del(ctx, "x", "y"); err == nil {
		t.Fatal("DEL")
	}
	if err := c.Set(ctx, "k", nil, 0); err == nil {
		t.Fatal("an error reply to SET was swallowed")
	}
}

func TestHandshake(t *testing.T) {
	ctx := context.Background()
	var seen []string
	f := serve(t, func(cmd []string) string {
		seen = append(seen, strings.Join(cmd, " "))
		switch cmd[0] {
		case "AUTH":
			if cmd[len(cmd)-1] != "good" {
				return "-WRONGPASS invalid\r\n"
			}
			return "+OK\r\n"
		case "SELECT":
			return "+OK\r\n"
		}
		return "+PONG\r\n"
	})
	c := f.client(redisdrv.Options{Username: "u", Password: "good", DB: 4})
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if strings.Join(seen, ";") != "AUTH u good;SELECT 4;PING" {
		t.Fatalf("handshake %q", seen)
	}
	c = f.client(redisdrv.Options{Password: "bad"})
	var re *redisdrv.Error
	if err := c.Ping(ctx); !errors.As(err, &re) || !strings.Contains(err.Error(), "WRONGPASS") {
		t.Fatalf("a refused AUTH: %v", err)
	}
	c.Close()
}

func TestTimeoutsAndCancellation(t *testing.T) {
	f := serve(t, func(cmd []string) string { return "HANG" })
	c := f.client(redisdrv.Options{IOTimeout: 50 * time.Millisecond})
	defer c.Close()
	start := time.Now()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("a server that never answers answered")
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	c2 := f.client(redisdrv.Options{IOTimeout: time.Minute})
	defer c2.Close()
	if err := c2.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled read: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeouts did not bound the wait")
	}
	if err := c2.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context took a slot: %v", err)
	}
}

func TestPoolBoundsAndClose(t *testing.T) {
	release := make(chan struct{})
	f := serve(t, func(cmd []string) string {
		if cmd[0] == "BLOCK" {
			<-release
		}
		return "+PONG\r\n"
	})
	c := f.client(redisdrv.Options{MaxOpen: 1, MaxIdle: 5})
	done := make(chan error)
	go func() { _, err := c.Do(context.Background(), "BLOCK"); done <- err }()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := c.Ping(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second caller beyond MaxOpen: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := c.Ping(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if f.dials.Load() != 1 {
		t.Fatalf("%d dials; the idle connection was not reused", f.dials.Load())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := c.Ping(context.Background()); !errors.Is(err, redisdrv.ErrClosed) {
		t.Fatalf("after Close: %v", err)
	}
}

func TestDialFailure(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	c := redisdrv.Open(redisdrv.Options{Addr: addr})
	defer c.Close()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("dialled a closed port")
	}
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("the failed dial leaked its slot or succeeded")
	}
}

func TestTLSHandshakeFailure(t *testing.T) {
	f := serve(t, func([]string) string { return "CLOSE" })
	o, _ := redisdrv.ParseURL("rediss://" + f.ln.Addr().String())
	c := redisdrv.Open(o)
	defer c.Close()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("TLS to a plaintext server succeeded")
	}
}
