package runtime

import (
	"errors"
	"testing"
)

// byteRows is the shape storm's own clients return: wire bytes, no Values.
type byteRows struct{ closed bool }

func (*byteRows) Next() bool          { return false }
func (*byteRows) RawValues() [][]byte { return nil }
func (r *byteRows) Close()            { r.closed = true }
func (*byteRows) Err() error          { return nil }

// valueRows is the shape a database/sql adapter returns.
type valueRows struct{ byteRows }

func (*valueRows) Values() []any { return nil }

func TestAsValueRowsPassesTheQueryErrorThrough(t *testing.T) {
	boom := errors.New("boom")
	if _, err := AsValueRows(nil, boom); err != boom {
		t.Errorf("got %v, want the Query's own error unchanged", err)
	}
}

func TestAsValueRowsAcceptsTheValueShape(t *testing.T) {
	in := &valueRows{}
	got, err := AsValueRows(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Error("the rows were not handed back as they came")
	}
	if in.closed {
		t.Error("rows of the right shape were closed")
	}
}

// The mis-wired package. Before ValueRows it scanned nil — Values was a method
// every adapter had, and a byte adapter's returned nothing — which is a silent
// failure. Now it is a named error, and the rows the caller will never see are
// closed rather than leaked with their connection.
func TestAsValueRowsRefusesAndClosesTheByteShape(t *testing.T) {
	in := &byteRows{}
	got, err := AsValueRows(in, nil)
	if !errors.Is(err, ErrByteRows) {
		t.Fatalf("got %v, want ErrByteRows", err)
	}
	if got != nil {
		t.Error("refused rows were handed back anyway")
	}
	if !in.closed {
		t.Error("refused rows were not closed, so their connection stays checked out")
	}
}

// Once per QUERY on the value path, and it must stay free: an assertion to an
// interface does not allocate, and a helper that started to would be a cost on
// every read an Oracle package makes.
func TestAsValueRowsAllocatesNothing(t *testing.T) {
	var in Rows = &valueRows{}
	if n := testing.AllocsPerRun(100, func() { _, _ = AsValueRows(in, nil) }); n != 0 {
		t.Errorf("AsValueRows allocates %.0f time(s) per query; the budget is 0", n)
	}
}
