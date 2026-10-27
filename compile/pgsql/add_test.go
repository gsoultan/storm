package pgsql_test

import (
	"testing"

	"github.com/gsoultan/storm/compile/pgsql"
)

// An add binds only the amount: the sum is the column's own value plus it, so
// two writers who add from one read both land.
func TestAddReadsTheColumnItWrites(t *testing.T) {
	if a, b := pgsql.AddFrag("hits"); a != `"hits" = "hits" + $` || b != "" {
		t.Errorf("AddFrag = %q, %q", a, b)
	}
}
