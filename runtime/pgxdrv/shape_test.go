package pgxdrv

// The row SHAPE contract, which is a promise a generated package relies on.
//
// A package generated for a value-shaped target asks its rows for
// runtime.ValueRows once per query and refuses them when they are not. pgx
// hands over the wire, so this adapter's rows must NOT be ValueRows: rows that
// grew a Values method returning nil would pass that check and then scan
// nothing — the loud failure turned back into a silent one.

import (
	"testing"

	"github.com/gsoultan/storm/runtime"
)

func TestThisAdapterIsTheByteShape(t *testing.T) {
	var r runtime.Rows = rows{}
	if _, ok := r.(runtime.ValueRows); ok {
		t.Error("pgx hands over the wire; these rows must not claim the value " +
			"shape, or a value-shaped package would scan nil instead of refusing them")
	}
}
