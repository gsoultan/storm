package storm_test

// storm.ShardedSQL is the escape hatch that still carries a shard. What is
// worth asserting is the part that closes the hole: the two forms disagree
// about whether they are sharded, and generate reads that disagreement.

import (
	"testing"

	"github.com/gsoultan/storm"
)

type agingRow struct {
	Bucket string
	Total  storm.Decimal
}

var (
	plainAging   = storm.SQL[agingRow](`SELECT bucket, total FROM invoices WHERE tenant_id = $1`)
	shardedAging = storm.ShardedSQL[agingRow](`SELECT bucket, total FROM invoices WHERE tenant_id = $1`)

	plainPurge   = storm.SQLExec(`DELETE FROM invoices WHERE tenant_id = $1`)
	shardedPurge = storm.ShardedSQLExec(`DELETE FROM invoices WHERE tenant_id = $1`)
)

func TestShardedOfDistinguishesTheTwoForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		decl storm.RawDecl
		want bool
	}{
		{"storm.SQL", plainAging, false},
		{"storm.ShardedSQL", shardedAging, true},
		{"storm.SQLExec", plainPurge, false},
		{"storm.ShardedSQLExec", shardedPurge, true},
	} {
		if got := storm.ShardedOf(tc.decl); got != tc.want {
			t.Errorf("ShardedOf(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The sharded form is validated exactly like the plain one: generate reads
// the same row type, the same text and the same argument count off it. If it
// did not, a sharded declaration would skip the PREPARE that makes the
// escape hatch typed at all.
func TestShardedDeclarationsCarryTheSameMetadata(t *testing.T) {
	pt, ps := storm.DeclOf(plainAging)
	st, ss := storm.DeclOf(shardedAging)
	if pt != st {
		t.Errorf("row types differ: %v vs %v", pt, st)
	}
	if ps != ss {
		t.Errorf("SQL differs:\n%q\n%q", ps, ss)
	}
	if storm.ArgsOf(plainAging) != storm.ArgsOf(shardedAging) {
		t.Errorf("argument counts differ: %d vs %d",
			storm.ArgsOf(plainAging), storm.ArgsOf(shardedAging))
	}

	// The no-rows half reports a nil row type, sharded or not — that is what
	// makes generate refuse a statement whose descriptor has columns.
	if rt, _ := storm.DeclOf(shardedPurge); rt != nil {
		t.Errorf("ShardedSQLExec reported row type %v, want nil", rt)
	}
}

func TestShardedSQLExposesItsText(t *testing.T) {
	if shardedAging.SQL() == "" || shardedPurge.SQL() == "" {
		t.Error("a sharded declaration does not report its own text")
	}
}
