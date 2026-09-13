package runtime_test

// The client-side key, for a target whose server cannot default one.
//
// PostgreSQL never reaches this: gen_random_uuid() fires and storm emits
// nothing. On MySQL and MariaDB the generated write path calls it, so an id
// that is not unique, not time-ordered or not random is a defect in the primary
// key of every table.

import (
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
)

func TestNewUUIDv7IsVersion7AndVariantRFC4122(t *testing.T) {
	u := runtime.NewUUIDv7()
	if got := u[6] >> 4; got != 7 {
		t.Errorf("version = %d, want 7", got)
	}
	if got := u[8] >> 6; got != 0b10 {
		t.Errorf("variant bits = %02b, want 10", got)
	}
}

// The point of v7 over v4: byte order IS time order, so a BINARY(16) primary
// key stays local in the index instead of scattering every insert. On MySQL the
// primary key is the clustered index, so this is a write-throughput property
// rather than a cosmetic one.
func TestNewUUIDv7SortsByCreationTime(t *testing.T) {
	first := runtime.NewUUIDv7()
	time.Sleep(2 * time.Millisecond)
	second := runtime.NewUUIDv7()
	if string(first[:6]) >= string(second[:6]) {
		t.Errorf("a later uuid does not sort after an earlier one:\n%x\n%x", first, second)
	}
	// The timestamp is the real clock, not a counter: a value minted now must
	// carry roughly now, or ordering across processes is meaningless.
	ms := int64(first[0])<<40 | int64(first[1])<<32 | int64(first[2])<<24 |
		int64(first[3])<<16 | int64(first[4])<<8 | int64(first[5])
	if d := time.Since(time.UnixMilli(ms)); d < 0 || d > time.Minute {
		t.Errorf("the embedded timestamp is %v away from now", d)
	}
}

func TestNewUUIDv7DoesNotRepeat(t *testing.T) {
	seen := make(map[[16]byte]bool, 10000)
	for i := 0; i < 10000; i++ {
		u := runtime.NewUUIDv7()
		if seen[u] {
			t.Fatalf("a uuid repeated after %d draws", i)
		}
		seen[u] = true
	}
}

// Two uuids minted in the same millisecond must still differ, or a burst of
// inserts collides on the primary key.
func TestNewUUIDv7DiffersWithinAMillisecond(t *testing.T) {
	a, b := runtime.NewUUIDv7(), runtime.NewUUIDv7()
	if a == b {
		t.Fatal("two uuids minted back to back are identical")
	}
	if string(a[:6]) == string(b[:6]) && string(a[6:]) == string(b[6:]) {
		t.Fatal("the random half did not vary")
	}
}

func TestIsZeroUUID(t *testing.T) {
	var zero [16]byte
	if !runtime.IsZeroUUID(zero) {
		t.Error("the zero value is not reported as zero")
	}
	if runtime.IsZeroUUID(runtime.NewUUIDv7()) {
		t.Error("a generated uuid was reported as zero")
	}
}
