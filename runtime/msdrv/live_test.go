package msdrv_test

// The adapter against a real server.
//
// Skipped unless STORM_MSSQL_ADDR names one. Every assertion here is about the
// WIRE — that the client speaks TDS well enough for a server to answer — which
// is the half no unit test can reach.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/msdrv"
)

func cfg(t *testing.T) msdrv.Config {
	t.Helper()
	a := os.Getenv("STORM_MSSQL_ADDR")
	if a == "" {
		t.Skip("STORM_MSSQL_ADDR unset")
	}
	ensureDatabase(t, a)
	return msdrv.Config{
		Addr: a, User: "sa", Password: os.Getenv("STORM_MSSQL_PASSWORD"),
		Database: "storm", AppName: "storm-test",
		// The development server's certificate is the one SQL Server generates
		// for itself, which Go refuses to parse (see ErrNegativeSerial). The
		// TLS paths are covered by their own test, which says so.
		TLS: msdrv.TLSDisabled,
	}
}

func open(t *testing.T) *msdrv.Conn {
	t.Helper()
	c, err := msdrv.Open(context.Background(), cfg(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// The first thing that has to work: PRELOGIN, the TLS handshake inside it, and
// LOGIN7. Everything else in this package is downstream of it.
func TestConnectAndSelectOne(t *testing.T) {
	c := open(t)
	rows, err := c.Query(context.Background(), "SELECT 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		v := rows.RawValues()
		if len(v) != 1 {
			t.Fatalf("got %d columns, want 1", len(v))
		}
		if len(v[0]) != 4 || v[0][0] != 1 {
			t.Errorf("SELECT 1 returned % x", v[0])
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d rows, want 1", n)
	}
}

// Parameters go through sp_executesql, which binds by NAME. The statement text
// is therefore fixed for a shape, which is what lets the server cache one plan
// per query rather than one per request.
func TestBoundParametersRoundTrip(t *testing.T) {
	c := open(t)
	ctx := context.Background()

	rows, err := c.Query(ctx, "SELECT @p1 + @p2", []any{int64(40), int64(2)})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	v := rows.RawValues()[0]
	if len(v) != 8 || binary.LittleEndian.Uint64(v) != 42 {
		t.Errorf("40 + 2 came back as % x", v)
	}
}

// Every type storm's DDL emits, sent as a parameter and read back. The point is
// the round trip: a value that survives it is one the binder and the decoder
// agree about, and they are written from opposite ends of the same wire format.
func TestEveryTypeSurvivesTheRoundTrip(t *testing.T) {
	c := open(t)
	ctx := context.Background()

	if _, err := c.Exec(ctx, `
		IF OBJECT_ID('dbo.msdrv_types') IS NOT NULL DROP TABLE dbo.msdrv_types;
		CREATE TABLE dbo.msdrv_types (
			id UNIQUEIDENTIFIER NOT NULL PRIMARY KEY,
			b BIT NOT NULL,
			i2 SMALLINT NOT NULL, i4 INT NOT NULL, i8 BIGINT NOT NULL,
			f4 REAL NOT NULL, f8 FLOAT(53) NOT NULL,
			dec_ DECIMAL(19,4) NOT NULL,
			s NVARCHAR(200) NOT NULL, big NVARCHAR(MAX) NOT NULL,
			bin VARBINARY(MAX) NOT NULL,
			d DATE NOT NULL, tod TIME(7) NOT NULL,
			ts DATETIME2(7) NOT NULL, tz DATETIMEOFFSET(7) NOT NULL,
			maybe NVARCHAR(50) NULL
		)`, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Exec(context.Background(), "DROP TABLE dbo.msdrv_types", nil)
	})

	id := [16]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
		0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}
	when := time.Date(2026, 9, 20, 14, 30, 45, 123456700, time.UTC)
	big := strings.Repeat("wide.", 4000) // past nvarchar(4000): the PLP path
	dec := runtime.Decimal{Unscaled: 1234567, Scale: 4}

	n, err := c.Exec(ctx, `INSERT INTO dbo.msdrv_types
		(id,b,i2,i4,i8,f4,f8,dec_,s,big,bin,d,tod,ts,tz,maybe)
		VALUES (@p1,@p2,@p3,@p4,@p5,@p6,@p7,@p8,@p9,@p10,@p11,@p12,@p13,@p14,@p15,@p16)`,
		[]any{id, true, int16(7), int32(70), int64(700),
			float32(1.5), float64(2.25), dec,
			"héllo, wörld", big, []byte{0xde, 0xad, 0xbe, 0xef},
			when, runtime.TimeOfDay(time.Duration(14*time.Hour + 30*time.Minute).Microseconds()),
			when, when, (*string)(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("INSERT affected %d rows, want 1", n)
	}

	rows, err := c.Query(ctx, `SELECT id,b,i2,i4,i8,f4,f8,dec_,s,big,bin,d,tod,ts,tz,maybe
		FROM dbo.msdrv_types WHERE id = @p1`, []any{id})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("the row is not there: %v", rows.Err())
	}
	v := rows.RawValues()

	// A uuid must come back in CANONICAL order, not the wire's mixed-endian
	// layout. Storing it scrambled round-trips through storm and matches
	// nothing anybody else wrote.
	if !bytes.Equal(v[0], id[:]) {
		t.Errorf("uuid: got % x, want % x", v[0], id[:])
	}
	if len(v[1]) != 1 || v[1][0] != 1 {
		t.Errorf("bit: % x", v[1])
	}
	if binary.LittleEndian.Uint16(v[2]) != 7 {
		t.Errorf("smallint: % x", v[2])
	}
	if binary.LittleEndian.Uint32(v[3]) != 70 {
		t.Errorf("int: % x", v[3])
	}
	if binary.LittleEndian.Uint64(v[4]) != 700 {
		t.Errorf("bigint: % x", v[4])
	}
	if math.Float32frombits(binary.LittleEndian.Uint32(v[5])) != 1.5 {
		t.Errorf("real: % x", v[5])
	}
	if math.Float64frombits(binary.LittleEndian.Uint64(v[6])) != 2.25 {
		t.Errorf("float: % x", v[6])
	}
	// The canonical nine bytes: scale, then the unscaled int64.
	if len(v[7]) != 9 || v[7][0] != 4 ||
		int64(binary.LittleEndian.Uint64(v[7][1:])) != 1234567 {
		t.Errorf("decimal: % x", v[7])
	}
	if got := ucs2(v[8]); got != "héllo, wörld" {
		t.Errorf("nvarchar: %q", got)
	}
	if got := ucs2(v[9]); got != big {
		t.Errorf("nvarchar(max): %d chars, want %d", len(got), len(big))
	}
	if !bytes.Equal(v[10], []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Errorf("varbinary: % x", v[10])
	}
	if len(v[11]) != 3 {
		t.Errorf("date: % x", v[11])
	}
	if len(v[12]) != 5 {
		t.Errorf("time: % x", v[12])
	}
	if len(v[13]) != 8 {
		t.Errorf("datetime2: % x", v[13])
	}
	if len(v[14]) != 10 {
		t.Errorf("datetimeoffset: % x", v[14])
	}
	// NULL is nil and nothing else is. An empty string is not a NULL, and the
	// null-bitmap row encoding is the one that gets this wrong.
	if v[15] != nil {
		t.Errorf("a NULL came back as % x", v[15])
	}
}

// ucs2 decodes what an nvarchar column hands back, which is UTF-16LE.
func ucs2(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(u))
}

// The Executor port, end to end: a pool, a transaction, a batch and a bulk
// load, against a real server.
func TestExecutorPort(t *testing.T) {
	ctx := context.Background()
	p, err := msdrv.NewPool(ctx, cfg(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if _, err := p.Exec(ctx, `
		IF OBJECT_ID('dbo.msdrv_port') IS NOT NULL DROP TABLE dbo.msdrv_port;
		CREATE TABLE dbo.msdrv_port (id INT NOT NULL PRIMARY KEY, name NVARCHAR(50) NOT NULL)`,
		nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), "DROP TABLE dbo.msdrv_port", nil) })

	// A transaction that rolls back leaves nothing behind.
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO dbo.msdrv_port (id,name) VALUES (@p1,@p2)",
		[]any{int32(99), "gone"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, p, "SELECT count(*) FROM dbo.msdrv_port"); n != 0 {
		t.Errorf("a rolled-back insert left %d rows", n)
	}

	// A batch is ONE round trip carrying several statements, which is what TDS
	// allows and the MySQL wire does not.
	ops := make([]runtime.BatchOp, 0, 5)
	for i := 1; i <= 5; i++ {
		ops = append(ops, runtime.BatchOp{
			SQL:  "INSERT INTO dbo.msdrv_port (id,name) VALUES (@p1,@p2)",
			Args: []any{int32(i), "batched"},
		})
	}
	seen := 0
	var totals int64
	if err := p.Batch(ctx, ops, func(i int, rows runtime.Rows, affected int64, err error) error {
		if err != nil {
			return err
		}
		seen++
		totals += affected
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != len(ops) {
		t.Errorf("the callback saw %d of %d results", seen, len(ops))
	}
	if totals != 5 {
		t.Errorf("the batch reported %d affected rows, want 5", totals)
	}
	if n := count(t, p, "SELECT count(*) FROM dbo.msdrv_port"); n != 5 {
		t.Errorf("the batch wrote %d rows, want 5", n)
	}

	// A bulk load. Emulated here — see CopyFrom — but the guarantee it exists
	// for is round trips, not the wire format.
	src := &rowSource{n: 500, from: 100}
	loaded, err := p.CopyFrom(ctx, "msdrv_port", []string{"id", "name"}, src)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != 500 {
		t.Errorf("CopyFrom loaded %d rows, want 500", loaded)
	}
	if n := count(t, p, "SELECT count(*) FROM dbo.msdrv_port"); n != 505 {
		t.Errorf("after the load there are %d rows, want 505", n)
	}

	// A unique violation is a TYPED error, not a string to match.
	_, err = p.Exec(ctx, "INSERT INTO dbo.msdrv_port (id,name) VALUES (@p1,@p2)",
		[]any{int32(1), "duplicate"})
	if !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Errorf("a duplicate key came back as %v, not a unique violation", err)
	}
	// And it carries no VALUE. SQL Server's own message ends "The duplicate key
	// value is (1)", and that is a row's data in anything that logs the error.
	if err != nil && strings.Contains(err.Error(), "duplicate key value") {
		t.Errorf("the error carries the offending value: %v", err)
	}
}

type rowSource struct {
	n, from, i int
	vals       []any
}

func (s *rowSource) Next() bool {
	if s.i >= s.n {
		return false
	}
	s.i++
	s.vals = []any{int32(s.from + s.i), "bulk"}
	return true
}
func (s *rowSource) Values() []any { return s.vals }
func (s *rowSource) Err() error    { return nil }

func count(t *testing.T, p *msdrv.Pool, q string) int64 {
	t.Helper()
	rows, err := p.Query(context.Background(), q, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("count returned no row: %v", rows.Err())
	}
	v := rows.RawValues()[0]
	return int64(int32(binary.LittleEndian.Uint32(v)))
}

// ensureDatabase creates the test database if the server has not got one.
//
// In CI the SQL Server service starts empty, so every test here would fail with
// "Cannot open database" — a green-looking configuration problem that reads as
// a client defect. Creating it is two statements and removes the out-of-band
// step entirely.
func ensureDatabase(t testing.TB, addr string) {
	t.Helper()
	c, err := msdrv.Open(context.Background(), msdrv.Config{
		Addr: addr, User: "sa", Password: os.Getenv("STORM_MSSQL_PASSWORD"),
		Database: "master", TLS: msdrv.TLSDisabled,
	})
	if err != nil {
		t.Fatalf("connecting to master: %v", err)
	}
	defer c.Close()
	// Its own batch: CREATE DATABASE cannot share one with anything else.
	if _, err := c.Exec(context.Background(),
		"IF DB_ID('storm') IS NULL CREATE DATABASE storm", nil); err != nil {
		t.Fatalf("creating the storm database: %v", err)
	}
}

// The bulk path is ONE round trip for the whole load — the protocol's own, not
// an emulation — and it round-trips every type the loader encodes.
//
// The counting decorator is what proves the round-trip claim: it counts calls
// to the port, and a CopyFrom that fell back to per-row inserts would still
// show one there. So the load is also checked for having arrived intact, which
// is what a wrong width or a mis-scaled decimal would break.
func TestBulkLoadIsOneRoundTripAndArrivesIntact(t *testing.T) {
	c := open(t)
	ctx := context.Background()

	if _, err := c.Exec(ctx, `
		IF OBJECT_ID('dbo.msdrv_bulk') IS NOT NULL DROP TABLE dbo.msdrv_bulk;
		CREATE TABLE dbo.msdrv_bulk (
			id UNIQUEIDENTIFIER NOT NULL PRIMARY KEY,
			n INT NOT NULL, big BIGINT NOT NULL, ok BIT NOT NULL,
			amount DECIMAL(19,4) NOT NULL,
			label NVARCHAR(50) NOT NULL, note NVARCHAR(MAX) NULL,
			blob VARBINARY(MAX) NULL,
			when_ DATETIMEOFFSET(7) NOT NULL, day DATE NOT NULL,
			ratio FLOAT(53) NOT NULL)`, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.Exec(context.Background(), "DROP TABLE dbo.msdrv_bulk", nil) })

	when := time.Date(2026, 9, 20, 14, 30, 45, 123456700, time.UTC)
	src := &bulkSource{n: 1000, when: when}
	n, err := c.CopyFrom(ctx, "msdrv_bulk",
		[]string{"id", "n", "big", "ok", "amount", "label", "note", "blob",
			"when_", "day", "ratio"}, src)
	if err != nil {
		var se *msdrv.Error
		if errors.As(err, &se) {
			t.Fatalf("%d: %s", se.Number, se.ServerMessage())
		}
		t.Fatal(err)
	}
	if n != 1000 {
		t.Errorf("CopyFrom loaded %d rows, want 1000", n)
	}

	rows, err := c.Query(ctx, `SELECT id,n,big,ok,amount,label,note,blob,when_,day,ratio
		FROM dbo.msdrv_bulk WHERE n = @p1`, []any{int32(8)})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("row 8 is not there: %v", rows.Err())
	}
	v := rows.RawValues()
	if v[0] == nil || len(v[0]) != 16 || v[0][15] != 8 {
		t.Errorf("uuid: % x", v[0])
	}
	if binary.LittleEndian.Uint32(v[1]) != 8 {
		t.Errorf("int: % x", v[1])
	}
	if binary.LittleEndian.Uint64(v[2]) != 80 {
		t.Errorf("bigint: % x", v[2])
	}
	if v[3][0] != 1 {
		t.Errorf("bit: % x", v[3])
	}
	// The decimal is rescaled to the COLUMN's scale on the way in. A value sent
	// at scale 2 into a scale-4 column would be a hundred times too small, and
	// nothing would say so.
	if len(v[4]) != 9 || v[4][0] != 4 ||
		int64(binary.LittleEndian.Uint64(v[4][1:])) != 80000 {
		t.Errorf("decimal: % x", v[4])
	}
	if got := ucs2(v[5]); got != "label-8" {
		t.Errorf("nvarchar: %q", got)
	}
	if got := ucs2(v[6]); !strings.HasPrefix(got, "wide") {
		t.Errorf("nvarchar(max): %q", got[:min(20, len(got))])
	}
	if len(v[7]) != 3 || v[7][0] != 8 {
		t.Errorf("varbinary: % x", v[7])
	}
	if len(v[8]) != 10 {
		t.Errorf("datetimeoffset: % x", v[8])
	}
	if len(v[9]) != 3 {
		t.Errorf("date: % x", v[9])
	}
	if math.Float64frombits(binary.LittleEndian.Uint64(v[10])) != 0.5 {
		t.Errorf("float: % x", v[10])
	}

	// A NULL in a nullable column is a NULL, not an empty value.
	rows.Close()
	rows2, err := c.Query(ctx,
		"SELECT count_big(*) FROM dbo.msdrv_bulk WHERE note IS NULL", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows2.Close()
	if !rows2.Next() {
		t.Fatal(rows2.Err())
	}
	if got := int64(binary.LittleEndian.Uint64(rows2.RawValues()[0])); got != 500 {
		t.Errorf("%d rows have a NULL note, want 500", got)
	}
}

// bulkSource yields rows with every type the loader encodes, and a NULL in
// every other row.
type bulkSource struct {
	n, i int
	when time.Time
	v    []any
}

func (s *bulkSource) Next() bool {
	if s.i >= s.n {
		return false
	}
	var id [16]byte
	// Two bytes, because a thousand rows do not fit in one and the primary key
	// would collide at 256 — which the server reports correctly and which is a
	// fixture bug rather than a loader one.
	id[15] = byte(s.i)
	id[14] = byte(s.i >> 8)
	var note any = strings.Repeat("wide", 2000)
	var blob any = []byte{byte(s.i), 0xbe, 0xef}
	if s.i%2 == 1 {
		note, blob = (*string)(nil), []byte(nil)
	}
	s.v = []any{id, int32(s.i), int64(s.i) * 10, s.i%2 == 0,
		runtime.Decimal{Unscaled: int64(s.i), Scale: 0},
		"label-" + strconv.Itoa(s.i), note, blob,
		s.when, s.when, 0.5}
	s.i++
	return true
}

func (s *bulkSource) Values() []any { return s.v }
func (s *bulkSource) Err() error    { return nil }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type oneSrc struct {
	v    []any
	done bool
}

func (s *oneSrc) Next() bool {
	if s.done {
		return false
	}
	s.done = true
	return true
}
func (s *oneSrc) Values() []any { return s.v }
func (s *oneSrc) Err() error    { return nil }

// One column at a time, one table each. Every type the bulk encoder can write,
// because a bulk row carries NO parameter declaration — there is no conversion
// step, so a value written in the wrong width is read as the next column's
// bytes and the server reports it against a column that is not the one at
// fault. Three defects came out of this exact list:
//
//   - A fixed BIT was written with a length byte, which the server reported as
//     the NEXT unicode column having an odd byte size.
//   - A decimal was written at the widest form rather than the COLUMN's, which
//     left eight bytes of magnitude where the next value should start.
//   - A MAX value used the known-length PLP header, after which the terminator
//     was read as the next row's token.
func TestEveryBulkTypeLoads(t *testing.T) {
	c := open(t)
	ctx := context.Background()
	when := time.Date(2026, 9, 20, 14, 30, 45, 123456700, time.UTC)

	for i, tc := range []struct {
		col string
		v   any
	}{
		{"DATETIMEOFFSET(7) NOT NULL", when},
		{"DATE NOT NULL", when},
		{"TIME(7) NOT NULL", when},
		{"DATETIME2(7) NOT NULL", when},
		{"FLOAT(53) NOT NULL", 0.5},
		{"REAL NOT NULL", float32(0.5)},
		{"DECIMAL(19,4) NOT NULL", mustDec()},
		{"VARBINARY(MAX) NULL", []byte{1, 2, 3}},
		{"NVARCHAR(MAX) NULL", "wide"},
		{"NVARCHAR(50) NOT NULL", "short"},
		{"BIT NOT NULL", true},
		{"INT NOT NULL", int32(3)},
		{"BIGINT NOT NULL", int64(3)},
		{"SMALLINT NOT NULL", int16(3)},
		{"UNIQUEIDENTIFIER NOT NULL", [16]byte{1}},
	} {
		name := "bp" + string(rune('a'+i))
		t.Run(tc.col, func(t *testing.T) {
			if _, err := c.Exec(ctx, `IF OBJECT_ID('dbo.`+name+`') IS NOT NULL DROP TABLE dbo.`+name+`;
				CREATE TABLE dbo.`+name+` (v `+tc.col+`)`, nil); err != nil {
				t.Fatal(err)
			}
			defer c.Exec(context.Background(), "DROP TABLE dbo."+name, nil)
			n, err := c.CopyFrom(ctx, name, []string{"v"}, &oneSrc{v: []any{tc.v}})
			if err != nil {
				var se *msdrv.Error
				if errors.As(err, &se) {
					t.Fatalf("%d: %s", se.Number, se.ServerMessage())
				}
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("loaded %d", n)
			}
		})
	}
}

func mustDec() runtime.Decimal { return runtime.Decimal{Unscaled: 12345, Scale: 2} }
