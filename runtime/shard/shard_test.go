package shard

// The key and the locators. These are the parts with no database behind them,
// and the parts whose mistakes are silent: a key that collides puts two
// tenants on one shard and nothing reports it, and a locator that moves keys
// when the shard count changes turns adding a shard into a data loss.

import (
	"errors"
	"testing"
)

func TestKeyShapesDoNotCollide(t *testing.T) {
	// The shape is mixed into the hash first for exactly this: without it,
	// a tenant slug of "1" and a legacy account id of 1 would share a shard
	// by accident, which is not wrong but hides a modelling mistake.
	if StringKey("1").hash() == Int64Key(1).hash() {
		t.Error(`StringKey("1") and Int64Key(1) hash the same`)
	}
	var u [16]byte
	if UUIDKey(u).hash() == Int64Key(0).hash() {
		t.Error("the zero UUID and Int64Key(0) hash the same")
	}
}

func TestKeyIsStableAcrossCalls(t *testing.T) {
	// A shard key that hashed differently between processes would route a
	// tenant to a different database after a deploy. This is the cheap
	// version of that assertion; the expensive one is that the algorithm is
	// written out here rather than taken from a map iteration order.
	k := StringKey("acme")
	for range 100 {
		if k.hash() != StringKey("acme").hash() {
			t.Fatal("the same key hashed two ways")
		}
	}
}

// Routing happens on the path of every query against a sharded table, so the
// key must not reach the heap. This is the assertion behind that claim.
func TestKeyHashingDoesNotAllocate(t *testing.T) {
	var sink uint64
	if n := testing.AllocsPerRun(200, func() {
		sink = StringKey("a-fairly-long-tenant-slug").hash()
	}); n != 0 {
		t.Errorf("StringKey hashing allocated %v times per run", n)
	}
	if n := testing.AllocsPerRun(200, func() {
		sink = UUIDKey([16]byte{1, 2, 3}).hash()
	}); n != 0 {
		t.Errorf("UUIDKey hashing allocated %v times per run", n)
	}
	_ = sink
}

func TestZeroKeyIsRefused(t *testing.T) {
	// The zero Key means no column was read into it — a caller who wrote
	// shard.Key{} or forgot to set the field. Placing it would send every
	// such row to one shard, consistently, and look like it worked.
	for _, loc := range []Locator{Jump(4), Modulo(4)} {
		if _, err := loc.Locate(Key{}); !errors.Is(err, ErrNoShard) {
			t.Errorf("%T placed the zero Key: %v", loc, err)
		}
	}
}

func TestJumpStaysInRange(t *testing.T) {
	const n = 7
	j := Jump(n)
	for i := range 1000 {
		id, err := j.Locate(Int64Key(int64(i)))
		if err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
		if id < 0 || int(id) >= n {
			t.Fatalf("key %d landed on shard %d, outside 0..%d", i, id, n-1)
		}
	}
}

// The reason to prefer Jump over Modulo is what growing costs. Going from 4
// shards to 5 should move about 1/5 of the keys; Modulo moves about 4/5. If
// this ever regresses, adding a shard silently becomes a full reshuffle.
func TestJumpMovesOnlyItsShareWhenAShardIsAdded(t *testing.T) {
	const keys = 20000
	moved := 0
	from, to := Jump(4), Jump(5)
	for i := range keys {
		k := Int64Key(int64(i))
		a, _ := from.Locate(k)
		b, _ := to.Locate(k)
		if a != b {
			moved++
		}
	}
	frac := float64(moved) / keys
	if frac < 0.15 || frac > 0.25 {
		t.Errorf("growing 4 → 5 moved %.1f%% of keys, want about 20%%", frac*100)
	}
}

func TestJumpSpreadsKeys(t *testing.T) {
	const (
		n    = 4
		keys = 20000
	)
	var hits [n]int
	for i := range keys {
		id, _ := Jump(n).Locate(Int64Key(int64(i)))
		hits[id]++
	}
	// A locator that sent everything to one shard would satisfy every other
	// test in this file.
	for id, h := range hits {
		if h < keys/n/2 || h > keys/n*2 {
			t.Errorf("shard %d took %d of %d keys, which is not a spread", id, h, keys)
		}
	}
}

func TestModuloStaysInRange(t *testing.T) {
	const n = 3
	for i := range 500 {
		id, err := Modulo(n).Locate(StringKey(string(rune('a' + i%26))))
		if err != nil {
			t.Fatal(err)
		}
		if id < 0 || int(id) >= n {
			t.Fatalf("shard %d outside 0..%d", id, n-1)
		}
	}
}

func TestEmptyLocatorIsRefused(t *testing.T) {
	for _, loc := range []Locator{Jump(0), Modulo(0), Jump(-1)} {
		if _, err := loc.Locate(Int64Key(1)); !errors.Is(err, ErrNoShard) {
			t.Errorf("%T over no shards placed a key: %v", loc, err)
		}
	}
}

func TestTableLooksKeysUp(t *testing.T) {
	east, west := StringKey("acme"), StringKey("globex")
	tbl, err := NewTable(4, map[Key]ID{east: 0, west: 3})
	if err != nil {
		t.Fatal(err)
	}
	if id, err := tbl.Locate(east); err != nil || id != 0 {
		t.Errorf("acme → %d, %v; want 0", id, err)
	}
	if id, err := tbl.Locate(west); err != nil || id != 3 {
		t.Errorf("globex → %d, %v; want 3", id, err)
	}
}

// A hash locator answers a typo with a real shard and a silently empty
// result. A lookup table says the key is not a tenant, and that is the reason
// to use one.
func TestTableRefusesAnUnknownKey(t *testing.T) {
	tbl, err := NewTable(2, map[Key]ID{StringKey("acme"): 0})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tbl.Locate(StringKey("acmee"))
	if !errors.Is(err, ErrNoShard) {
		t.Fatalf("err = %v, want ErrNoShard", err)
	}
	if got := err.Error(); !contains(got, "acmee") {
		t.Errorf("the refusal does not name the key: %s", got)
	}
}

// Checked once, at construction, rather than on the query that happens to use
// the bad entry — which could be months later and on one tenant.
func TestTableRefusesAnOutOfRangeShardAtConstruction(t *testing.T) {
	if _, err := NewTable(2, map[Key]ID{StringKey("acme"): 5}); err == nil {
		t.Error("a key mapped to shard 5 of 2 was accepted")
	}
	if _, err := NewTable(2, map[Key]ID{StringKey("acme"): -1}); err == nil {
		t.Error("a key mapped to shard -1 was accepted")
	}
	if _, err := NewTable(0, nil); err == nil {
		t.Error("a Table over no shards was accepted")
	}
	if _, err := NewTable(2, map[Key]ID{{}: 0}); err == nil {
		t.Error("the zero Key was accepted into the lookup table")
	}
}

// The key appears in refusals, so it has to render as the thing the reader
// typed into their config.
func TestKeyStringIsReadable(t *testing.T) {
	if got := StringKey("acme").String(); got != "acme" {
		t.Errorf("String() = %q", got)
	}
	if got := Int64Key(42).String(); got != "42" {
		t.Errorf("String() = %q", got)
	}
	u := [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	if got := UUIDKey(u).String(); got != "01020304-0506-0708-090a-0b0c0d0e0f10" {
		t.Errorf("String() = %q, want the canonical uuid form", got)
	}
	if got := (Key{}).String(); got != "<invalid>" {
		t.Errorf("String() = %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
