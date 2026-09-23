package shard

// The Set and the Unit: routing, single-shard transactions, and the refusal
// that is the whole answer to "what happens on a cross-shard write".

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

type fakeTx struct {
	db        *fakeDB
	commits   int
	rollbacks int
	commitErr error
}

func (t *fakeTx) Query(context.Context, string, []any) (runtime.Rows, error) { return nil, nil }
func (t *fakeTx) Exec(context.Context, string, []any) (int64, error)         { return 0, nil }
func (t *fakeTx) CopyFrom(context.Context, string, []string, runtime.CopySource) (int64, error) {
	return 0, nil
}
func (t *fakeTx) Batch(context.Context, []runtime.BatchOp, func(int, runtime.Rows, int64, error) error) error {
	return nil
}
func (t *fakeTx) Commit(context.Context) error   { t.commits++; return t.commitErr }
func (t *fakeTx) Rollback(context.Context) error { t.rollbacks++; return nil }

type fakeDB struct {
	name     string
	begins   int
	tx       *fakeTx
	beginErr error
}

func (d *fakeDB) Query(context.Context, string, []any) (runtime.Rows, error) { return nil, nil }
func (d *fakeDB) Exec(context.Context, string, []any) (int64, error)         { return 0, nil }
func (d *fakeDB) CopyFrom(context.Context, string, []string, runtime.CopySource) (int64, error) {
	return 0, nil
}
func (d *fakeDB) Batch(context.Context, []runtime.BatchOp, func(int, runtime.Rows, int64, error) error) error {
	return nil
}
func (d *fakeDB) Begin(context.Context) (runtime.Tx, error) {
	d.begins++
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	if d.tx == nil {
		d.tx = &fakeTx{db: d}
	}
	return d.tx, nil
}

func dbs(n int) []runtime.DB {
	out := make([]runtime.DB, n)
	for i := range out {
		out[i] = &fakeDB{name: string(rune('a' + i))}
	}
	return out
}

// A locator built for four shards over a set holding three would resolve keys
// to a database that is not there — on whichever tenant happened to hash high,
// months after the deploy. Caught at construction instead.
func TestNewRefusesAMiscountedSet(t *testing.T) {
	_, err := New(Jump(4), dbs(3)...)
	if err == nil {
		t.Fatal("a 4-shard locator over 3 shards was accepted")
	}
	if !strings.Contains(err.Error(), "4") || !strings.Contains(err.Error(), "3") {
		t.Errorf("the refusal names neither count: %v", err)
	}
}

func TestNewRefusesTheDegenerateCases(t *testing.T) {
	if _, err := New(nil, dbs(1)...); err == nil {
		t.Error("New accepted a nil locator")
	}
	if _, err := New(Jump(0)); err == nil {
		t.Error("New accepted no shards")
	}
	if _, err := New(Jump(2), &fakeDB{}, nil); err == nil {
		t.Error("New accepted a nil shard")
	}
}

func TestForRoutesToOneShard(t *testing.T) {
	shards := dbs(4)
	s, err := New(Jump(4), shards...)
	if err != nil {
		t.Fatal(err)
	}

	k := StringKey("acme")
	want, _ := Jump(4).Locate(k)

	ex, err := s.For(k)
	if err != nil {
		t.Fatal(err)
	}
	if ex.Shard() != want {
		t.Errorf("Shard() = %d, want %d", ex.Shard(), want)
	}
	// Twice, because a Set that routed by round-robin would pass the check
	// above and be catastrophically wrong.
	again, _ := s.For(k)
	if again.Shard() != want {
		t.Errorf("the same key routed to %d then %d", want, again.Shard())
	}
}

func TestForIDIsBoundsChecked(t *testing.T) {
	s, _ := New(Jump(2), dbs(2)...)
	for _, id := range []ID{-1, 2, 99} {
		if _, err := s.ForID(id); !errors.Is(err, ErrNoShard) {
			t.Errorf("ForID(%d) = %v, want ErrNoShard", id, err)
		}
	}
	if _, err := s.ForID(1); err != nil {
		t.Errorf("ForID(1): %v", err)
	}
}

func TestBeginOpensOnTheKeysShardOnly(t *testing.T) {
	shards := dbs(4)
	s, _ := New(Jump(4), shards...)

	k := StringKey("acme")
	id, _ := Jump(4).Locate(k)

	tx, err := s.Begin(t.Context(), k)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Shard() != id {
		t.Errorf("the transaction is on shard %d, the key is on %d", tx.Shard(), id)
	}
	for i, db := range shards {
		got := db.(*fakeDB).begins
		want := 0
		if ID(i) == id {
			want = 1
		}
		if got != want {
			t.Errorf("shard %d: %d Begins, want %d", i, got, want)
		}
	}
}

func TestInTxCommitsOnTheOneShard(t *testing.T) {
	s, _ := New(Modulo(3), dbs(3)...)
	k := Int64Key(7)
	id, _ := Modulo(3).Locate(k)

	var ran Bound
	if err := s.InTx(t.Context(), k, func(ex Bound) error { ran = ex; return nil }); err != nil {
		t.Fatal(err)
	}
	if ran.Shard() != id {
		t.Errorf("fn ran on shard %d, want %d", ran.Shard(), id)
	}
}

func TestInTxRollsBackOnErrorAndOnPanic(t *testing.T) {
	boom := errors.New("boom")

	s, _ := New(Modulo(1), dbs(1)...)
	if err := s.InTx(t.Context(), Int64Key(1), func(Bound) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}

	s2, _ := New(Modulo(1), dbs(1)...)
	func() {
		defer func() {
			if p := recover(); p != "kaboom" {
				t.Errorf("recovered %v, want the original panic", p)
			}
		}()
		_ = s2.InTx(t.Context(), Int64Key(1), func(Bound) error { panic("kaboom") })
		t.Error("InTx swallowed a panic")
	}()
}

func TestEachVisitsEveryShardInOrder(t *testing.T) {
	s, _ := New(Jump(3), dbs(3)...)

	var seen []ID
	if err := s.Each(t.Context(), func(_ context.Context, id ID, ex Bound) error {
		if ex.Shard() != id {
			t.Errorf("shard %d was handed an executor bound to %d", id, ex.Shard())
		}
		seen = append(seen, id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0] != 0 || seen[1] != 1 || seen[2] != 2 {
		t.Errorf("visited %v, want 0,1,2 in order", seen)
	}
}

func TestEachStopsAtTheFirstErrorAndNamesTheShard(t *testing.T) {
	s, _ := New(Jump(4), dbs(4)...)
	boom := errors.New("backfill failed")

	visited := 0
	err := s.Each(t.Context(), func(_ context.Context, id ID, _ Bound) error {
		visited++
		if id == 2 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if !strings.Contains(err.Error(), "shard 2") {
		t.Errorf("the error does not say which shard failed: %v", err)
	}
	if visited != 3 {
		t.Errorf("visited %d shards, want 3 — Each kept going after a failure", visited)
	}
}

func TestEachHonoursCancellation(t *testing.T) {
	s, _ := New(Jump(4), dbs(4)...)
	ctx, cancel := context.WithCancel(t.Context())

	visited := 0
	err := s.Each(ctx, func(_ context.Context, _ ID, _ Bound) error {
		visited++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if visited != 1 {
		t.Errorf("visited %d shards after cancellation, want 1", visited)
	}
}

func TestPinMakesABoundFromAnyExecutor(t *testing.T) {
	// The door that lets a CountingExecutor reach a sharded model's methods,
	// which is what proves the N+1 guarantee in an adopter's own suite.
	ce := &runtime.CountingExecutor{Inner: &fakeDB{}}
	b := Pin(2, ce)
	if b.Shard() != 2 {
		t.Errorf("Shard() = %d, want 2", b.Shard())
	}
	if _, err := b.Exec(t.Context(), "select 1", nil); err != nil {
		t.Fatal(err)
	}
	if n := ce.RoundTrips(); n != 1 {
		t.Errorf("the wrapped executor saw %d round trips, want 1", n)
	}
}

// The cross-shard refusal, which is the whole answer storm gives to a write
// that spans two databases. It has to fire at Add, while the code that chose
// the keys is still on the stack.
func TestUnitRefusesASecondShard(t *testing.T) {
	rank := map[string]int{"orders": 0, "order_lines": 1}
	u := NewUnit(rank)

	east, west := Pin(0, &fakeDB{}), Pin(3, &fakeDB{})

	if err := u.Add(east, "orders", runtime.BatchOp{SQL: "insert into orders ..."}); err != nil {
		t.Fatalf("the first Add was refused: %v", err)
	}
	err := u.Add(west, "orders", runtime.BatchOp{SQL: "insert into orders ..."})
	if !errors.Is(err, ErrCrossShard) {
		t.Fatalf("err = %v, want ErrCrossShard", err)
	}
	for _, want := range []string{"shard 0", "shard 3", "orders"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	// And nothing was staged, so the caller can carry on with a split unit.
	if u.Len() != 1 {
		t.Errorf("Len() = %d after a refused Add, want 1", u.Len())
	}
}

func TestUnitAcceptsManyWritesOnOneShard(t *testing.T) {
	u := NewUnit(map[string]int{"orders": 0, "order_lines": 1})
	ex := Pin(1, &fakeDB{})

	for _, tbl := range []string{"orders", "order_lines", "order_lines"} {
		if err := u.Add(ex, tbl, runtime.BatchOp{SQL: "insert ..."}); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
	}
	if u.Len() != 3 {
		t.Errorf("Len() = %d, want 3", u.Len())
	}
	if id, ok := u.Shard(); !ok || id != 1 {
		t.Errorf("Shard() = %d, %v; want 1, true", id, ok)
	}
}

// Staged against a pool-backed Bound, flushed against a transaction-backed
// one: legitimate, and the shard has to match.
func TestUnitRefusesAFlushAimedElsewhere(t *testing.T) {
	u := NewUnit(map[string]int{"orders": 0})
	if err := u.Add(Pin(1, &fakeDB{}), "orders", runtime.BatchOp{SQL: "insert ..."}); err != nil {
		t.Fatal(err)
	}

	_, err := u.Flush(t.Context(), Pin(2, &fakeDB{}))
	if !errors.Is(err, ErrCrossShard) {
		t.Fatalf("err = %v, want ErrCrossShard", err)
	}
	if !strings.Contains(err.Error(), "shard 1") || !strings.Contains(err.Error(), "shard 2") {
		t.Errorf("the refusal does not name both shards: %v", err)
	}
}

func TestEmptyUnitHasNoShard(t *testing.T) {
	u := NewUnit(map[string]int{"orders": 0})
	if _, ok := u.Shard(); ok {
		t.Error("an empty unit claims a shard")
	}
	if _, err := u.Flush(t.Context(), Pin(9, &fakeDB{})); err != nil {
		t.Errorf("flushing an empty unit: %v", err)
	}
}
