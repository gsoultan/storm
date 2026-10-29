package author_test

// The generated scanner against a database that has drifted from the model.
//
// Offline on purpose: Scan is wire bytes to struct, so a drifted row can be
// written by hand. Before the guard these two rows indexed past the slice and
// PANICKED, inside whatever request ran the query.

import (
	"errors"
	"strings"
	"testing"

	"github.com/gsoultan/storm/examples/blog/store/author"
	"github.com/gsoultan/storm/runtime"
)

func TestScanRefusesADriftedRowInsteadOfPanicking(t *testing.T) {
	id := make([]byte, 16)
	ts := make([]byte, 8)
	good := [][]byte{id, ts, ts, []byte("Ada"), []byte("ada@example.com")}

	var sl runtime.Slab
	var r author.Row
	if err := author.Scan(good, &r, &sl); err != nil {
		t.Fatalf("a well-shaped row: %v", err)
	}

	cases := map[string][][]byte{
		// created_at NULL where the model says NOT NULL: a DROP NOT NULL by hand.
		"null in a not-null column": {id, nil, ts, []byte("Ada"), []byte("ada@example.com")},
		// created_at as four bytes: the column's type changed underneath the model.
		"wrong width": {id, ts[:4], ts, []byte("Ada"), []byte("ada@example.com")},
	}
	for name, rv := range cases {
		t.Run(name, func(t *testing.T) {
			var r author.Row
			err := author.Scan(rv, &r, &sl)
			if !errors.Is(err, runtime.ErrShape) {
				t.Fatalf("err = %v, want runtime.ErrShape", err)
			}
			if !strings.Contains(err.Error(), "created_at") || !strings.Contains(err.Error(), "timestamptz") {
				t.Fatalf("the error does not name the column and its type: %v", err)
			}
		})
	}
}
