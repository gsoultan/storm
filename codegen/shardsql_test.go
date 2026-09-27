package codegen

// The escape hatch is the one door out of sharding's compile-time guarantee,
// so the scan that closes it is worth testing on its own — especially the
// boundary cases, where a miss is a raw query running on the wrong database
// and reporting one shard's rows as all of them.

import (
	"strings"
	"testing"

	"github.com/gsoultan/storm/schema"
)

func shardedSchema() *schema.Schema {
	return &schema.Schema{Tables: []*schema.Table{
		{Name: "orders", ShardKey: "tenant_id", Columns: []*schema.Column{
			{Name: "tenant_id", Type: schema.Type{Name: schema.TypeUUID}, NotNull: true},
		}},
		{Name: "currencies", Columns: []*schema.Column{
			{Name: "code", Type: schema.Type{Name: schema.TypeText}, NotNull: true},
		}},
	}}
}

func TestPlainRawSQLNamingAShardedTableIsRefused(t *testing.T) {
	err := refuseUnshardedRawSQL(shardedSchema(), []string{
		"SELECT id FROM orders WHERE tenant_id = $1",
	})
	if err == nil {
		t.Fatal("a plain storm.SQL reading a sharded table was accepted")
	}
	for _, want := range []string{"orders", "tenant_id", "storm.ShardedSQL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
}

func TestPlainRawSQLOnUnshardedTablesIsFine(t *testing.T) {
	if err := refuseUnshardedRawSQL(shardedSchema(), []string{
		"SELECT code FROM currencies",
		"DELETE FROM currencies WHERE code = $1",
	}); err != nil {
		t.Fatalf("an unsharded statement was refused: %v", err)
	}
}

// A sharded declaration never reaches the list, so an empty list is the
// "everything was declared correctly" case and must not refuse.
func TestNoPlainStatementsIsNotARefusal(t *testing.T) {
	if err := refuseUnshardedRawSQL(shardedSchema(), nil); err != nil {
		t.Fatalf("no plain statements refused: %v", err)
	}
}

func TestUnshardedSchemaNeverRefuses(t *testing.T) {
	s := &schema.Schema{Tables: []*schema.Table{{Name: "orders"}}}
	if err := refuseUnshardedRawSQL(s, []string{"SELECT * FROM orders"}); err != nil {
		t.Fatalf("an unsharded schema refused a raw statement: %v", err)
	}
}

// The boundary cases. `orders` inside `work_orders` is a different table, and
// missing that would refuse a legitimate statement; matching only at a word
// start would miss `JOIN orders`.
func TestIdentifierMatchingRespectsBoundaries(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		want bool
	}{
		{"SELECT * FROM orders", true},
		{"SELECT * FROM ORDERS", true},            // SQL is case-insensitive
		{"SELECT * FROM public.orders o", true},   // qualified
		{"... JOIN orders ON ...", true},          // mid-statement
		{`INSERT INTO "orders" VALUES (1)`, true}, // quoted
		{"orders", true},                          // whole text
		{"SELECT * FROM work_orders", false},      // longer identifier
		{"SELECT * FROM orders_archive", false},   // longer the other way
		{"SELECT * FROM myorders", false},         //
		{"SELECT reorders FROM t", false},         //
		{"SELECT * FROM currencies", false},       // unrelated
	} {
		if got := namesIdentifier(tc.sql, "orders"); got != tc.want {
			t.Errorf("namesIdentifier(%q) = %v, want %v", tc.sql, got, tc.want)
		}
	}
}

// A false positive is a build error naming the fix; a false negative is a
// wrong answer in production. When the text mentions the table twice, once
// legitimately and once not, refusing is the right answer.
func TestASecondOccurrenceStillMatches(t *testing.T) {
	if !namesIdentifier("SELECT * FROM work_orders JOIN orders ON ...", "orders") {
		t.Error("a real occurrence after a substring one was missed")
	}
}

func TestEmptyNameNeverMatches(t *testing.T) {
	if namesIdentifier("SELECT 1", "") {
		t.Error("the empty table name matched")
	}
}

// The refusal has to identify WHICH declaration without printing a
// hundred-line query into a build log.
func TestRefusalQuotesOnlyTheHeadOfALongStatement(t *testing.T) {
	long := "SELECT a\nFROM orders\nWHERE b\nAND c\nAND d\nAND e\nAND f"
	err := refuseUnshardedRawSQL(shardedSchema(), []string{long})
	if err == nil {
		t.Fatal("not refused")
	}
	if !strings.Contains(err.Error(), "...") {
		t.Error("a long statement was not elided")
	}
	if strings.Contains(err.Error(), "AND f") {
		t.Error("the whole statement was printed into the refusal")
	}
}
