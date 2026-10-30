package mydrv_test

// Production failures, reproduced against real servers: values past the
// protocol's size boundaries, a time zone that is not UTC, and a server that
// drops connections the pool is holding. Each of these passed every test in
// this package before it was written, and each corrupted data or failed
// statements on both MySQL 8.4 and MariaDB 11.4.

import (
	"context"
	"crypto/tls"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime/mydec"
	"github.com/gsoultan/storm/runtime/mydrv"
)

// onBoth runs f against each server that is configured.
func onBoth(t *testing.T, f func(t *testing.T, cfg mydrv.Config)) {
	t.Run("mysql", func(t *testing.T) { f(t, config(t)) })
	t.Run("mariadb", func(t *testing.T) { f(t, mariaConfig(t)) })
}

func openCfg(t *testing.T, cfg mydrv.Config) *mydrv.Conn {
	t.Helper()
	c, err := mydrv.Open(context.Background(), cfg)
	if err != nil {
		t.Skipf("no server: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// maxPacket is the server's max_allowed_packet.
func maxPacket(t *testing.T, c *mydrv.Conn) int {
	n, err := strconv.Atoi(one(t, c, "SELECT CAST(@@max_allowed_packet AS CHAR)"))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A bound value is stored whole at every length-prefix width, and past the
// 16 MiB at which one packet becomes several. A 70,000-byte value used to be
// stored as its first 4,464 bytes with no error at all.
func TestLargeBoundValuesArriveWhole(t *testing.T) {
	onBoth(t, func(t *testing.T, cfg mydrv.Config) {
		c := openCfg(t, cfg)
		limit := maxPacket(t, c)
		for _, n := range []int{250, 251, 65535, 65536, 70000, 1 << 20, 1<<24 - 1, 1 << 24, 17 << 20} {
			if 2*n+1024 > limit {
				continue // the statement carries the value twice
			}
			v := strings.Repeat("x", n-1) + "!"
			r, err := c.Query(context.Background(), "SELECT CAST(LENGTH(?) AS CHAR), ?", []any{v, v})
			if err != nil {
				t.Fatalf("%d bytes: %v", n, err)
			}
			if !r.Next() {
				t.Fatalf("%d bytes: no row: %v", n, r.Err())
			}
			seen, back := string(r.RawValues()[0]), string(r.RawValues()[1])
			r.Close()
			if seen != strconv.Itoa(n) {
				t.Errorf("%d bytes bound, the server saw %s", n, seen)
			}
			if back != v {
				t.Errorf("%d bytes bound, %d read back", n, len(back))
			}
		}
	})
}

// A ROW of 16 MiB or more arrives as several packets, and is read as one.
func TestLargeRowsAreReassembled(t *testing.T) {
	onBoth(t, func(t *testing.T, cfg mydrv.Config) {
		c := openCfg(t, cfg)
		const n = 17 << 20
		if maxPacket(t, c) < n+1024 {
			t.Skip("max_allowed_packet is below the size under test")
		}
		r, err := c.Query(context.Background(), "SELECT REPEAT('y', ?)", []any{int64(n)})
		if err != nil {
			t.Fatal(err)
		}
		if !r.Next() {
			t.Fatalf("no row: %v", r.Err())
		}
		if v := r.RawValues()[0]; len(v) != n || strings.Trim(string(v), "y") != "" {
			t.Fatalf("read a %d-byte value, want %d bytes of y", len(v), n)
		}
		r.Close()
		if got := one(t, c, "SELECT CAST(1 AS CHAR)"); got != "1" {
			t.Fatalf("the connection is unusable after a large row: %q", got)
		}
	})
}

// A value past max_allowed_packet is the server's error, by number, and the
// pool discards the connection the server closed rather than handing it out.
func TestAValuePastMaxAllowedPacketIsNamedAndCostsOneConnection(t *testing.T) {
	onBoth(t, func(t *testing.T, cfg mydrv.Config) {
		cfg.MaxConns = 1
		p, err := mydrv.NewPool(context.Background(), cfg)
		if err != nil {
			t.Skipf("no server: %v", err)
		}
		defer p.Close()
		n, _ := strconv.Atoi(one(t, p, "SELECT CAST(@@max_allowed_packet AS CHAR)"))
		_, err = p.Exec(context.Background(), "SELECT LENGTH(?)", []any{strings.Repeat("z", n+1)})
		var me *mydrv.Error
		if !errors.As(err, &me) || me.Code != 1153 {
			t.Fatalf("err = %v, want the server's 1153", err)
		}
		if got := one(t, p, "SELECT CAST(2 AS CHAR)"); got != "2" {
			t.Fatalf("the pool handed out the closed connection: %q", got)
		}
	})
}

// A time is stored as the instant it names, whatever zone it carries and
// whatever zone the server runs in; and the server's own clock agrees with
// the client's. Before, both depended on time zones: a bound value was stored
// in its own zone's wall clock, and CURRENT_TIMESTAMP(6) in the server's.
func TestTimesAreInstantsWhateverTheZones(t *testing.T) {
	onBoth(t, func(t *testing.T, cfg mydrv.Config) {
		admin := openCfg(t, cfg)
		// A server whose zone is not UTC — the deployment this went wrong on.
		// Set globally, so it is what a NEW session starts with.
		was := one(t, admin, "SELECT @@global.time_zone")
		if _, err := admin.Exec(context.Background(), "SET GLOBAL time_zone = '+05:00'", nil); err != nil {
			t.Skipf("cannot set the server's zone: %v", err)
		}
		defer admin.Exec(context.Background(), "SET GLOBAL time_zone = '"+was+"'", nil)

		c := openCfg(t, cfg)
		in := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.FixedZone("UTC+7", 7*3600))
		r, err := c.Query(context.Background(), "SELECT CAST(? AS DATETIME(6)), CURRENT_TIMESTAMP(6)", []any{in})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if !r.Next() {
			t.Fatal(r.Err())
		}
		got, err := mydec.DateTime(r.RawValues()[0])
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(in) {
			t.Errorf("bound %v, read back %v", in, got)
		}
		now, err := mydec.DateTime(r.RawValues()[1])
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(now); d > time.Minute || d < -time.Minute {
			t.Errorf("the server's CURRENT_TIMESTAMP is %v from the client's clock", d)
		}
	})
}

// killConn ends a connection from the server side, as a restart, a failover
// or wait_timeout would, and waits until the server has let it go.
func killConn(t *testing.T, cfg mydrv.Config, id string) {
	t.Helper()
	side := openCfg(t, cfg)
	if _, err := side.Exec(context.Background(), "KILL "+id, nil); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		if one(t, side, "SELECT CAST(COUNT(*) AS CHAR) FROM information_schema.processlist WHERE id = "+id) == "0" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the server did not drop the connection")
}

// A connection the server dropped while it sat in the pool is noticed before
// it is handed out, so the statement that would have been given it runs.
// Before, it failed with EOF — and so did every statement after it, because
// a failure that was not a server error did not mark the connection broken.
func TestAConnectionTheServerDroppedIsNotHandedOut(t *testing.T) {
	defer mydrv.ProbeIdleAfter(0)()
	onBoth(t, func(t *testing.T, cfg mydrv.Config) {
		cfg.MaxConns = 1
		p, err := mydrv.NewPool(context.Background(), cfg)
		if err != nil {
			t.Skipf("no server: %v", err)
		}
		defer p.Close()
		killConn(t, cfg, one(t, p, "SELECT CAST(CONNECTION_ID() AS CHAR)"))
		if got := one(t, p, "SELECT CAST(3 AS CHAR)"); got != "3" {
			t.Fatalf("got %q", got)
		}
	})
}

// Without the probe — a connection used a moment ago — the failure costs ONE
// statement, not every statement after it.
func TestADeadConnectionCostsOneStatement(t *testing.T) {
	defer mydrv.ProbeIdleAfter(time.Hour)()
	onBoth(t, func(t *testing.T, cfg mydrv.Config) {
		cfg.MaxConns = 1
		p, err := mydrv.NewPool(context.Background(), cfg)
		if err != nil {
			t.Skipf("no server: %v", err)
		}
		defer p.Close()
		killConn(t, cfg, one(t, p, "SELECT CAST(CONNECTION_ID() AS CHAR)"))
		if _, err := p.Exec(context.Background(), "SELECT 1", nil); err == nil {
			t.Fatal("a statement on a connection the server killed succeeded")
		}
		for i := range 3 {
			if _, err := p.Exec(context.Background(), "SELECT 1", nil); err != nil {
				t.Fatalf("statement %d after the failure: %v", i+1, err)
			}
		}
	})
}

// The idle probe reads through TLS as well as plaintext, and a healthy
// encrypted connection survives it: a timed-out read must not corrupt the
// tunnel's state.
func TestTheIdleProbeLeavesATLSConnectionUsable(t *testing.T) {
	defer mydrv.ProbeIdleAfter(0)()
	cfg := config(t)
	cfg.TLS = mydrv.TLSRequired
	cfg.TLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // container cert
	cfg.MaxConns = 1
	p, err := mydrv.NewPool(context.Background(), cfg)
	if errors.Is(err, mydrv.ErrTLSUnsupported) {
		t.Skip("this server offers no TLS")
	}
	if err != nil {
		t.Skipf("no server: %v", err)
	}
	defer p.Close()
	id := one(t, p, "SELECT CAST(CONNECTION_ID() AS CHAR)")
	for i := range 5 {
		if got := one(t, p, "SELECT CAST(CONNECTION_ID() AS CHAR)"); got != id {
			t.Fatalf("probe %d replaced a healthy TLS connection (%s, was %s)", i+1, got, id)
		}
	}
}
