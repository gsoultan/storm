package pgxdrv

// The row SHAPE contract, which is a promise a generated package relies on.
//
// runtime.Rows has two accessors and an adapter implements exactly one; the
// other returns nil. A generated package calls whichever its dialect chose at
// GENERATE time, so nothing asks at run time — which means a byte adapter that
// started returning values here would not fail loudly, it would return rows
// nobody reads.

import "testing"

func TestThisAdapterIsTheByteShape(t *testing.T) {
	var r rows
	if r.Values() != nil {
		t.Error("pgx hands over the wire; Values must be nil so a generated " +
			"package cannot silently scan the wrong side of the port")
	}
}
