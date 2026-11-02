package mgdrv_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/bsondec"
	"github.com/gsoultan/storm/runtime/mgdrv"
)

// The live suite. It needs a MongoDB 6.0+ REPLICA SET — transactions refuse a
// standalone server — named by STORM_MONGO_URI, e.g.
//
//	docker run -d -p 27017:27017 mongo:7 --replSet rs0
//	docker exec <id> mongosh --eval 'rs.initiate({_id:"rs0",members:[{_id:0,host:"localhost:27017"}]})'
//	STORM_MONGO_URI=mongodb://localhost:27017/storm_test go test ./runtime/mgdrv
func pool(t *testing.T) *mgdrv.Pool {
	t.Helper()
	uri := os.Getenv("STORM_MONGO_URI")
	if uri == "" {
		t.Skip("STORM_MONGO_URI is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := mgdrv.OpenURI(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func fresh(t *testing.T, p *mgdrv.Pool, coll string) {
	t.Helper()
	ctx := context.Background()
	_, _ = p.Exec(ctx, `{"drop":"`+coll+`"}`, nil)
	if _, err := p.Exec(ctx, `{"create":"`+coll+`"}`, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLiveRoundTrip(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	fresh(t, p, "rt")
	if _, err := p.Exec(ctx, `{"createIndexes":"rt","indexes":[{"key":{"email":1},"name":"uq_rt_email","unique":true}]}`, nil); err != nil {
		t.Fatal(err)
	}
	id := runtime.NewUUIDv7()
	at := time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.UTC)
	n, err := p.Exec(ctx, `{"insert":"rt","documents":[{"_id":{"$arg":1},"email":{"$arg":2},"n":{"$arg":3},"at":{"$arg":4},"d":{"$arg":5},"f":{"$arg":6},"ok":{"$arg":7},"gone":{"$arg":8}}]}`,
		[]any{id, "a@x", int64(1) << 40, at, runtime.Decimal{Unscaled: -12345, Scale: 2}, 1.5, true, nil})
	if err != nil || n != 1 {
		t.Fatalf("insert: %d %v", n, err)
	}

	rs, err := p.Query(ctx, `{"find":"rt","filter":{"email":{"$eq":{"$arg":1}}},"projection":{"_id":1,"email":1,"n":1,"at":1,"d":1,"f":1,"ok":1,"gone":1,"missing":1}}`, []any{"a@x"})
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	if !rs.Next() {
		t.Fatalf("no row: %v", rs.Err())
	}
	rv := rs.RawValues()
	if got, _ := bsondec.UUID(rv[0]); got != id {
		t.Errorf("_id = %x", got)
	}
	if got, _ := bsondec.Str(rv[1]); got != "a@x" {
		t.Errorf("email = %q", got)
	}
	if got, _ := bsondec.Int8(rv[2]); got != 1<<40 {
		t.Errorf("n = %d", got)
	}
	if got, _ := bsondec.Time(rv[3]); !got.Equal(at) {
		t.Errorf("at = %v", got)
	}
	if got, err := bsondec.Decimal(rv[4]); err != nil || got != (runtime.Decimal{Unscaled: -12345, Scale: 2}) {
		t.Errorf("d = %+v %v", got, err)
	}
	if got, _ := bsondec.Float8(rv[5]); got != 1.5 {
		t.Errorf("f = %v", got)
	}
	if got, _ := bsondec.Bool(rv[6]); !got {
		t.Error("ok = false")
	}
	if rv[7] != nil || rv[8] != nil {
		t.Errorf("null and missing fields must be nil, got %v %v", rv[7], rv[8])
	}
	if rs.Next() {
		t.Error("a second row")
	}

	// A duplicate is the same typed error every other back end returns.
	_, err = p.Exec(ctx, `{"insert":"rt","documents":[{"email":{"$arg":1}}]}`, []any{"a@x"})
	var ce *runtime.ConstraintError
	if !errors.Is(err, runtime.ErrUniqueViolation) || !errors.As(err, &ce) || ce.Constraint != "uq_rt_email" {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestLiveCursorAcrossBatches(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	fresh(t, p, "many")
	src := &counter{n: 2500}
	n, err := p.CopyFrom(ctx, "many", []string{"_id", "v"}, src)
	if err != nil || n != 2500 {
		t.Fatalf("copy: %d %v", n, err)
	}
	rs, err := p.Query(ctx, `{"find":"many","filter":{},"sort":{"_id":1},"projection":{"v":1}}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for rs.Next() {
		v, err := bsondec.Int8(rs.RawValues()[0])
		if err != nil || v != int64(seen)*10 {
			t.Fatalf("row %d: %d %v", seen, v, err)
		}
		seen++
	}
	rs.Close()
	if rs.Err() != nil || seen != 2500 {
		t.Fatalf("read %d rows: %v", seen, rs.Err())
	}

	// Stopping early kills the cursor and frees the connection: with a pool
	// of one, the next query would otherwise wait forever.
	rs, err = p.Query(ctx, `{"find":"many","filter":{},"batchSize":10,"projection":{"v":1}}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	rs.Next()
	rs.Close()
	c, err := p.Query(ctx, `{"aggregate":"many","cursor":{},"pipeline":[{"$match":{}},{"$count":"n"}]}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.Next() {
		t.Fatal(c.Err())
	}
	if n, _ := bsondec.Int8(c.RawValues()[0]); n != 2500 {
		t.Fatalf("count = %d", n)
	}
}

type counter struct{ i, n int }

func (c *counter) Next() bool    { c.i++; return c.i <= c.n }
func (c *counter) Values() []any { return []any{int64(c.i), int64(c.i-1) * 10} }
func (c *counter) Err() error    { return nil }

func TestLiveTransactions(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	fresh(t, p, "txn")
	ins := `{"insert":"txn","documents":[{"_id":{"$arg":1}}]}`

	if err := runtime.InTx(ctx, p, func(ex runtime.Executor) error {
		_, err := ex.Exec(ctx, ins, []any{int64(1)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := runtime.InTx(ctx, p, func(ex runtime.Executor) error {
		if _, err := ex.Exec(ctx, ins, []any{int64(2)}); err != nil {
			return err
		}
		// Visible inside the transaction…
		rs, err := ex.Query(ctx, `{"find":"txn","filter":{"_id":2},"projection":{"_id":1}}`, nil)
		if err != nil {
			return err
		}
		ok := rs.Next()
		rs.Close()
		if !ok {
			t.Error("the transaction cannot read its own write")
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// …and gone after the rollback.
	n, err := p.Exec(ctx, `{"delete":"txn","deletes":[{"q":{},"limit":0}]}`, nil)
	if err != nil || n != 1 {
		t.Fatalf("after commit and rollback, %d documents (want 1): %v", n, err)
	}
	for i := 0; i < 3; i++ { // the session's transaction number keeps climbing
		if err := runtime.InTx(ctx, p, func(ex runtime.Executor) error {
			_, err := ex.Exec(ctx, ins, []any{int64(10 + i)})
			return err
		}); err != nil {
			t.Fatal(i, err)
		}
	}
}

func TestLiveCancellation(t *testing.T) {
	p := pool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.Exec(ctx, `{"eval":"sleep"}`, nil) // refused fast, not a hang
	if err == nil {
		t.Fatal("eval should be refused")
	}
	_, err = p.Query(ctx, `{"aggregate":1,"cursor":{},"pipeline":[{"$documents":[{"x":1}]},{"$project":{"x":{"$function":{"body":"function(){var d=Date.now();while(Date.now()-d<3000){};return 1}","args":[],"lang":"js"}}}}]}`, nil)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("cancellation did not interrupt: %v after %v", err, time.Since(start))
	}
	// The pool recovers: the abandoned connection was discarded.
	if _, err := p.Exec(context.Background(), `{"ping":1}`, nil); err != nil {
		t.Fatal(err)
	}

}

func TestLiveBatchAndFindAndModify(t *testing.T) {
	p := pool(t)
	ctx := context.Background()
	fresh(t, p, "fam")
	ops := []runtime.BatchOp{
		{SQL: `{"insert":"fam","documents":[{"_id":1,"n":1},{"_id":2,"n":2}]}`},
		{SQL: `{"findAndModify":"fam","query":{"_id":{"$arg":1}},"update":{"$inc":{"n":{"$arg":2}}},"new":true,"fields":{"n":1}}`,
			Args: []any{int64(1), int64(10)}, WantRows: true},
		{SQL: `{"update":"fam","updates":[{"q":{},"u":{"$set":{"m":true}},"multi":true}]}`},
	}
	var got []string
	err := p.Batch(ctx, ops, func(i int, rs runtime.Rows, n int64, err error) error {
		if err != nil {
			return err
		}
		if rs != nil {
			for rs.Next() {
				v, _ := bsondec.Int8(rs.RawValues()[0])
				got = append(got, "row "+strconv.FormatInt(v, 10))
			}
			return rs.Err()
		}
		got = append(got, "n "+strconv.FormatInt(n, 10))
		return nil
	})
	if err != nil || strings.Join(got, ";") != "n 2;row 11;n 2" {
		t.Fatalf("%v %v", got, err)
	}
	// A nil callback still runs everything and reports the first failure.
	err = p.Batch(ctx, []runtime.BatchOp{{SQL: `{"insert":"fam","documents":[{"_id":1}]}`}, {SQL: `{"ping":1}`}}, nil)
	if !errors.Is(err, runtime.ErrUniqueViolation) {
		t.Fatal(err)
	}
	// findAndModify through Exec reports what it found; no match is no row.
	n, err := p.Exec(ctx, `{"findAndModify":"fam","query":{"_id":2},"remove":true}`, nil)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	rs, err := p.Query(ctx, `{"findAndModify":"fam","query":{"_id":99},"update":{"$set":{"x":1}},"fields":{"x":1}}`, nil)
	if err != nil || rs.Next() {
		t.Fatalf("a findAndModify that matched nothing has no row: %v", err)
	}
	rs.Close()
	if _, err := p.Query(ctx, `{"ping":1}`, nil); err == nil {
		t.Error("a command with no result shape is not a Query")
	}
	if _, err := p.Query(ctx, `{"insert":"fam","documents":[],"projection":{"a":1}}`, nil); err == nil {
		t.Error("insert does not return documents")
	}

	tx, err := p.StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Batch(ctx, []runtime.BatchOp{{SQL: `{"insert":"fam","documents":[{"_id":100}]}`}}, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := tx.CopyFrom(ctx, "fam", []string{"_id"}, &counter{i: 1000, n: 1003}); err != nil || n != 3 {
		t.Fatalf("CopyFrom in a transaction: %d %v", n, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); !errors.Is(err, runtime.ErrTxDone) {
		t.Error("committing twice")
	}
	if _, err := tx.Exec(ctx, `{"ping":1}`, nil); !errors.Is(err, runtime.ErrTxDone) {
		t.Error("Exec after commit")
	}
	if _, err := tx.Query(ctx, `{"ping":1}`, nil); !errors.Is(err, runtime.ErrTxDone) {
		t.Error("Query after commit")
	}
	if err := tx.Batch(ctx, nil, nil); !errors.Is(err, runtime.ErrTxDone) {
		t.Error("Batch after commit")
	}
	if _, err := tx.CopyFrom(ctx, "fam", nil, &counter{}); !errors.Is(err, runtime.ErrTxDone) {
		t.Error("CopyFrom after commit")
	}
	// A transaction that ran nothing ends without a round trip.
	empty, _ := p.StartTx(ctx)
	if err := empty.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// An argument with no BSON form sends nothing.
	if _, err := p.Exec(ctx, `{"insert":"fam","documents":[{"_id":{"$arg":1}}]}`, []any{struct{}{}}); err == nil {
		t.Fatal("an unsupported argument was sent")
	}
	if _, err := p.CopyFrom(ctx, "fam", []string{"_id"}, &badSource{}); err == nil {
		t.Fatal("an unsupported value was copied")
	}
}

type badSource struct{ done bool }

func (b *badSource) Next() bool    { d := b.done; b.done = true; return !d }
func (b *badSource) Values() []any { return []any{struct{}{}} }
func (b *badSource) Err() error    { return nil }

func TestLiveClosedPoolAndBadAddress(t *testing.T) {
	p := pool(t)
	p.Close()
	if _, err := p.Exec(context.Background(), `{"ping":1}`, nil); !errors.Is(err, mgdrv.ErrClosed) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := mgdrv.Open(ctx, mgdrv.Config{Addr: "127.0.0.1:1", Database: "x"}); err == nil {
		t.Fatal("dialled nothing")
	}
	if _, err := mgdrv.Open(ctx, mgdrv.Config{Addr: "127.0.0.1:1"}); err == nil {
		t.Fatal("a database is required")
	}
	if _, err := mgdrv.OpenURI(ctx, "nope://"); err == nil {
		t.Fatal("bad URI")
	}
}
