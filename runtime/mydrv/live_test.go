package mydrv_test

// Tests that need a real server, because what they check is the server's
// opinion: that TLS is actually negotiated, that a cancelled query is actually
// gone, and that the prepared-statement count actually stops growing. Each of
// those can be faked by a client that only inspects itself.
//
// Skipped unless STORM_MYSQL_ADDR (or STORM_MARIADB_ADDR) names a server.

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/mydrv"
)

func mariaConfig(t testing.TB) mydrv.Config {
	a := os.Getenv("STORM_MARIADB_ADDR")
	if a == "" {
		t.Skip("STORM_MARIADB_ADDR unset")
	}
	return mydrv.Config{Addr: a, User: "root", Password: "storm", Database: "storm"}
}

// one runs a single-column query and returns the first row's value as text.
func one(t *testing.T, e runtime.Executor, sql string) string {
	t.Helper()
	r, err := e.Query(context.Background(), sql, nil)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer r.Close()
	if !r.Next() {
		if err := r.Err(); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return ""
	}
	v := r.RawValues()
	return string(v[len(v)-1])
}

// The server's own opinion of whether the connection is encrypted. A client
// that reports its own flag proves nothing.
func TestTLSTunnelIsRealAccordingToTheServer(t *testing.T) {
	cfg := config(t)
	cfg.TLS = mydrv.TLSRequired
	// The container's certificate is self-signed by the server's own CA, so
	// verification is switched off HERE, in the test, by name — which is
	// exactly the choice the driver refuses to make on the caller's behalf.
	cfg.TLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // container cert
	c, err := mydrv.Open(context.Background(), cfg)
	if errors.Is(err, mydrv.ErrTLSUnsupported) {
		t.Skip("this server offers no TLS, so there is no tunnel to inspect")
	}
	if err != nil {
		t.Fatalf("TLS connect: %v", err)
	}
	defer c.Close()
	if got := one(t, c, "SHOW SESSION STATUS LIKE 'Ssl_cipher'"); got == "" {
		t.Fatal("the server reports no cipher, so the connection is not encrypted")
	}
	// ...and the connection still works for ordinary statements.
	if got := one(t, c, "SELECT CAST(42 AS CHAR)"); got != "42" {
		t.Errorf("a statement through the tunnel returned %q, want 42", got)
	}
}

// A nil TLSConfig must VERIFY. A driver that quietly skipped verification would
// turn TLSRequired into a decoration: encrypted to whoever answered.
func TestNilTLSConfigVerifiesTheCertificate(t *testing.T) {
	cfg := config(t)
	cfg.TLS = mydrv.TLSRequired
	cfg.TLSConfig = nil
	_, err := mydrv.Open(context.Background(), cfg)
	if errors.Is(err, mydrv.ErrTLSUnsupported) {
		t.Skip("this server offers no TLS, so there is no certificate to verify")
	}
	if err == nil {
		t.Fatal("connected to a self-signed server with verification on")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("err = %v, want a certificate error", err)
	}
}

// Cancelling must stop the statement on the SERVER. A socket deadline stops
// this process waiting and leaves the query running, holding its locks.
func TestCancelKillsTheQueryOnTheServer(t *testing.T) {
	p, err := mydrv.NewPool(context.Background(), config(t))
	if err != nil {
		t.Skipf("no server: %v", err)
	}
	defer p.Close()

	// A marker unique to this run, so the count below sees only OUR statement.
	// Without it the assertion reads the whole server and fails on somebody
	// else's sleeping query — including one left by a previous failed run.
	mark := "cancelprobe" + itoa(int(time.Now().UnixNano()%1e9))
	sleep := "SELECT SLEEP(30) /* " + mark + " */"

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(400 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err = p.Query(ctx, sleep, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("returned after %v; the cancellation did not take", elapsed)
	}
	// The claim under test: the server is no longer running it. A deadline
	// would leave SLEEP(30) in the process list for another 29 seconds.
	if got := one(t, p, "SELECT CAST(COUNT(*) AS CHAR) FROM information_schema.processlist "+
		// Excluding this connection, because the counting statement carries the
		// marker in its own text and would otherwise find itself.
		"WHERE id <> CONNECTION_ID() AND info LIKE '%"+mark+"%'"); got != "0" {
		t.Fatalf("%s statements still running on the server after cancellation", got)
	}
}

// A connection whose query was killed must still be usable — that is the whole
// reason to kill rather than to break the socket.
func TestTheConnectionSurvivesACancellation(t *testing.T) {
	c, err := mydrv.Open(context.Background(), config(t))
	if err != nil {
		t.Skipf("no server: %v", err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := c.Query(ctx, "SELECT SLEEP(30)", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if got := one(t, c, "SELECT CAST(7 AS CHAR)"); got != "7" {
		t.Errorf("the connection is unusable after a cancellation: %q", got)
	}
}

func TestPoolServesConcurrentGoroutines(t *testing.T) {
	cfg := config(t)
	cfg.MaxConns = 4
	p, err := mydrv.NewPool(context.Background(), cfg)
	if err != nil {
		t.Skipf("no server: %v", err)
	}
	defer p.Close()

	const goroutines, each = 16, 20
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*each)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				r, err := p.Query(context.Background(),
					"SELECT CAST(? AS CHAR), CAST(? AS CHAR)", []any{int64(g), int64(i)})
				if err != nil {
					errs <- err
					return
				}
				if !r.Next() {
					errs <- errors.New("no row")
					r.Close()
					return
				}
				v := r.RawValues()
				// Each goroutine must get ITS OWN arguments back. A pool that
				// let two goroutines share a socket would interleave here.
				if string(v[0]) != itoa(g) || string(v[1]) != itoa(i) {
					errs <- errors.New("got " + string(v[0]) + "," + string(v[1]) +
						" want " + itoa(g) + "," + itoa(i))
				}
				r.Close()
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A transaction must be pinned to one connection: BEGIN is session state, so a
// transaction run through the pool would open on one socket and commit on
// another.
func TestTransactionIsPinnedAndIsolated(t *testing.T) {
	p, err := mydrv.NewPool(context.Background(), config(t))
	if err != nil {
		t.Skipf("no server: %v", err)
	}
	defer p.Close()
	ctx := context.Background()
	mustPool(t, p, "DROP TABLE IF EXISTS `tx_probe`")
	mustPool(t, p, "CREATE TABLE `tx_probe` (`id` BIGINT PRIMARY KEY) ENGINE=InnoDB")

	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO `tx_probe` VALUES (?)", []any{int64(1)}); err != nil {
		t.Fatal(err)
	}
	if got := one(t, tx, "SELECT CAST(COUNT(*) AS CHAR) FROM `tx_probe`"); got != "1" {
		t.Errorf("inside the transaction: %q rows, want 1", got)
	}
	// Another connection must not see it yet. If Begin had not pinned, this
	// would already be committed and the test would pass for the wrong reason.
	if got := one(t, p, "SELECT CAST(COUNT(*) AS CHAR) FROM `tx_probe`"); got != "0" {
		t.Errorf("outside the uncommitted transaction: %q rows, want 0", got)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := one(t, p, "SELECT CAST(COUNT(*) AS CHAR) FROM `tx_probe`"); got != "0" {
		t.Errorf("after rollback: %q rows, want 0", got)
	}
	if _, err := tx.Exec(ctx, "SELECT 1", nil); !errors.Is(err, mydrv.ErrTxDone) {
		t.Errorf("a finished transaction accepted a statement: %v", err)
	}
}

// The statement cache is keyed by SQL TEXT, which is not a closed set. Without a
// bound, a query whose text varies exhausts the server's max_prepared_stmt_count
// and every prepare on the server starts failing — including other clients'.
func TestPreparedStatementCacheIsBounded(t *testing.T) {
	cfg := config(t)
	cfg.MaxPreparedStmts = 8
	c, err := mydrv.Open(context.Background(), cfg)
	if err != nil {
		t.Skipf("no server: %v", err)
	}
	defer c.Close()
	before := one(t, c, "SHOW GLOBAL STATUS LIKE 'Prepared_stmt_count'")
	for i := 0; i < 200; i++ {
		if _, err := c.Query(context.Background(),
			"SELECT CAST("+itoa(i)+" AS CHAR) /* distinct text */", nil); err != nil {
			t.Fatal(err)
		}
	}
	after := one(t, c, "SHOW GLOBAL STATUS LIKE 'Prepared_stmt_count'")
	// The bound is 8 plus the two SHOW statements; anything near 200 means the
	// cache never evicted.
	if atoi(after)-atoi(before) > 20 {
		t.Fatalf("prepared statements went %s -> %s over 200 distinct texts", before, after)
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func mustPool(t *testing.T, p *mydrv.Pool, sql string) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql, nil); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// caching_sha2_password's FULL AUTH path, against a real server.
//
// The fast path is what every connection after the first takes, so a driver can
// pass every ordinary test without ever running this code. Full auth happens
// once per account, before the server has cached the hash — which means it
// happens on the FIRST connection an adopter ever makes, and a bug here looks
// like "storm cannot connect to MySQL at all".
//
// Forced by creating an account the server has never seen. Over a plaintext
// socket, so the branch under test is the RSA one rather than cleartext.
func TestFullAuthOverPlaintextAgainstARealServer(t *testing.T) {
	admin := open(t)
	ctx := context.Background()
	user := "storm_fa" + itoa(int(time.Now().UnixNano()%1e6))
	mustExec(t, admin, "CREATE USER '"+user+"'@'%' IDENTIFIED WITH "+
		"caching_sha2_password BY 'fullauth'")
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP USER '"+user+"'@'%'", nil) })
	mustExec(t, admin, "GRANT SELECT ON storm.* TO '"+user+"'@'%'")

	cfg := config(t)
	cfg.User, cfg.Password = user, "fullauth"
	cfg.TLS = mydrv.TLSDisabled

	// Without consent it must refuse, and name why.
	cfg.AllowCleartextPasswordOverPlaintext = false
	if _, err := mydrv.Open(ctx, cfg); !errors.Is(err, mydrv.ErrCleartextRefused) {
		t.Fatalf("err = %v, want ErrCleartextRefused", err)
	}

	// With it, the RSA exchange has to actually work.
	cfg.AllowCleartextPasswordOverPlaintext = true
	c, err := mydrv.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("full auth over plaintext: %v", err)
	}
	defer c.Close()
	if got := one(t, c, "SELECT CAST(11 AS CHAR)"); got != "11" {
		t.Errorf("the connection does not work after full auth: %q", got)
	}
}

// ...and the SECOND connection takes the fast path, which is a different branch
// (AuthMoreData 0x03) and the one every connection after the first uses.
func TestFastAuthAfterTheServerHasCachedTheHash(t *testing.T) {
	admin := open(t)
	ctx := context.Background()
	user := "storm_fp" + itoa(int(time.Now().UnixNano()%1e6))
	mustExec(t, admin, "CREATE USER '"+user+"'@'%' IDENTIFIED WITH "+
		"caching_sha2_password BY 'fastpath'")
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP USER '"+user+"'@'%'", nil) })
	mustExec(t, admin, "GRANT SELECT ON storm.* TO '"+user+"'@'%'")

	cfg := config(t)
	cfg.User, cfg.Password = user, "fastpath"
	cfg.TLS = mydrv.TLSDisabled
	cfg.AllowCleartextPasswordOverPlaintext = true
	first, err := mydrv.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("first connection: %v", err)
	}
	first.Close()

	// Now the hash is cached, so this must succeed WITHOUT the consent flag —
	// there is nothing recoverable on the wire this time.
	cfg.AllowCleartextPasswordOverPlaintext = false
	second, err := mydrv.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("fast path: %v", err)
	}
	defer second.Close()
	if got := one(t, second, "SELECT CAST(12 AS CHAR)"); got != "12" {
		t.Errorf("the fast-path connection does not work: %q", got)
	}
}

// A wrong password must fail as an authentication error, not a hang and not a
// connection that half works.
func TestWrongPasswordIsAnError(t *testing.T) {
	cfg := config(t)
	cfg.Password = "definitely-not-the-password"
	_, err := mydrv.Open(context.Background(), cfg)
	if err == nil {
		t.Fatal("connected with the wrong password")
	}
	if !strings.Contains(err.Error(), "denied") && !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("err = %v, want an authentication failure", err)
	}
}

// MariaDB still defaults to mysql_native_password, so the same driver has to
// authenticate both ways. This is the half MySQL 8.4 no longer exercises.
//
// It also uses the DEFAULT TLS mode against a server with a self-signed
// certificate, which is the case that would fail if TLSPreferred verified: the
// default mode has to connect to a stock server.
func TestMariaDBNativePasswordStillWorks(t *testing.T) {
	c, err := mydrv.Open(context.Background(), mariaConfig(t))
	if err != nil {
		t.Fatalf("MariaDB connect: %v", err)
	}
	defer c.Close()
	if got := one(t, c, "SELECT CAST(1 AS CHAR)"); got != "1" {
		t.Errorf("SELECT 1 = %q", got)
	}
}
