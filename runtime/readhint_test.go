package runtime_test

import (
	"testing"

	"github.com/gsoultan/storm/runtime"
)

// A statement is shared by every LIMIT, because the limit is a bound argument.
// Before ReadHint scaled, a 20-row page read right after a 1,000-row export of
// the same filter reserved the export's whole arena: 50 KB for 20 rows.
func TestReadHintScalesToTheLimit(t *testing.T) {
	var st runtime.Stmt
	st.ObserveRead(1000, 41000)

	cases := []struct {
		limit       int64
		rows, bytes int
	}{
		{limit: 20, rows: 20, bytes: 820},
		{limit: 1, rows: 1, bytes: 41},
		{limit: 3, rows: 3, bytes: 123},
		// At or above what was seen: the observation as it is.
		{limit: 1000, rows: 1000, bytes: 41000},
		{limit: 5000, rows: 1000, bytes: 41000},
		// No usable limit: unscaled.
		{limit: 0, rows: 1000, bytes: 41000},
		{limit: -1, rows: 1000, bytes: 41000},
	}
	for _, c := range cases {
		r, b := st.ReadHint(c.limit)
		if r != c.rows || b != c.bytes {
			t.Errorf("ReadHint(%d) = %d rows, %d bytes; want %d, %d", c.limit, r, b, c.rows, c.bytes)
		}
	}
	if got := st.SlabHint(); got != 41000 {
		t.Errorf("SlabHint = %d, want the observed 41000", got)
	}
}

// Scaling rounds the bytes up, so a page of rows that each carry a few bytes
// still reserves something rather than zero and a growth on the first string.
func TestReadHintRoundsBytesUp(t *testing.T) {
	var st runtime.Stmt
	st.ObserveRead(1000, 10)
	if _, b := st.ReadHint(1); b != 1 {
		t.Errorf("bytes = %d, want 1", b)
	}
}

// ObserveSlab is the reader that does not count rows (Each, and any package
// generated before ObserveRead existed): it must size the arena as it always
// did, and the slice not at all.
func TestObserveSlabCarriesNoRowCount(t *testing.T) {
	var st runtime.Stmt
	st.ObserveRead(1000, 41000)
	st.ObserveSlab(512)
	r, b := st.ReadHint(20)
	if r != 0 || b != 512 {
		t.Errorf("after ObserveSlab: ReadHint = %d rows, %d bytes; want 0, 512", r, b)
	}
	if st.SlabHint() != 512 {
		t.Errorf("SlabHint = %d, want 512", st.SlabHint())
	}
}

// Each half saturates at 32 bits rather than wrapping into the other.
func TestObserveReadClamps(t *testing.T) {
	var st runtime.Stmt
	st.ObserveRead(-5, -1)
	if r, b := st.ReadHint(10); r != 0 || b != 0 {
		t.Errorf("negative observation: %d, %d; want 0, 0", r, b)
	}
	st.ObserveRead(7, 1<<40)
	if r, b := st.ReadHint(0); r != 7 || b != 1<<32-1 {
		t.Errorf("oversized bytes: %d rows, %d bytes; want 7, %d", r, b, 1<<32-1)
	}
	if st.SlabHint() != 1<<32-1 {
		t.Errorf("SlabHint = %d, want the saturated half", st.SlabHint())
	}
}

func TestGrow(t *testing.T) {
	// Enough room already: the same backing array, untouched.
	buf := make([]int, 2, 10)
	buf[0], buf[1] = 1, 2
	if got := runtime.Grow(buf, 8); &got[0] != &buf[0] || len(got) != 2 {
		t.Error("Grow reallocated a slice that had room")
	}
	// Not enough: the contents carried over, the room made, the length kept.
	got := runtime.Grow(buf, 50)
	if len(got) != 2 || cap(got) < 52 || got[0] != 1 || got[1] != 2 {
		t.Errorf("Grow = len %d cap %d %v", len(got), cap(got), got)
	}
	// From nil, the one allocation List makes for its result.
	if g := runtime.Grow[int](nil, 1000); len(g) != 0 || cap(g) != 1000 {
		t.Errorf("Grow(nil, 1000) = len %d cap %d", len(g), cap(g))
	}
	if g := runtime.Grow[int](nil, 0); g != nil {
		t.Error("Grow(nil, 0) allocated")
	}
	allocs := testing.AllocsPerRun(100, func() { _ = runtime.Grow(buf, 8) })
	if allocs != 0 {
		t.Errorf("Grow with room allocates %v times", allocs)
	}
}
