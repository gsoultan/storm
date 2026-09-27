package tool

// The wiring between a declaration and the refusal.
//
// codegen refuses a plain raw statement that names a sharded table, and that
// refusal is unit-tested there. What is NOT tested there is whether the
// generate command ever hands it the right list — and a refusal that is never
// given anything to refuse passes every test it has while checking nothing.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gsoultan/storm"
)

func TestPlainRawStatementsExcludesTheShardedForms(t *testing.T) {
	plainQ := storm.SQL[struct{ N int32 }](`SELECT n FROM orders WHERE tenant_id = $1`)
	plainX := storm.SQLExec(`DELETE FROM orders WHERE tenant_id = $1`)
	shardQ := storm.ShardedSQL[struct{ N int32 }](`SELECT n FROM orders WHERE tenant_id = $1`)
	shardX := storm.ShardedSQLExec(`DELETE FROM orders WHERE tenant_id = $1`)

	prev := RawQueries
	t.Cleanup(func() { RawQueries = prev })
	RawQueries = []storm.RawDecl{plainQ, shardQ, plainX, shardX}

	got := plainRawStatements()
	if len(got) != 2 {
		t.Fatalf("got %d plain statements, want 2:\n%v", len(got), got)
	}
	for _, s := range got {
		if !strings.Contains(s, "orders") {
			t.Errorf("unexpected statement: %q", s)
		}
	}
	// The two that came back must be the two PLAIN ones. They share their text
	// with the sharded ones on purpose — the difference is the declaration,
	// not the SQL — so this counts rather than matches.
	if n := strings.Count(strings.Join(got, "\n"), "SELECT"); n != 1 {
		t.Errorf("got %d SELECTs among the plain statements, want 1", n)
	}
	if n := strings.Count(strings.Join(got, "\n"), "DELETE"); n != 1 {
		t.Errorf("got %d DELETEs among the plain statements, want 1", n)
	}
}

// Every declaration sharded means nothing to refuse, which must be an empty
// list rather than everything.
func TestPlainRawStatementsIsEmptyWhenAllAreSharded(t *testing.T) {
	prev := RawQueries
	t.Cleanup(func() { RawQueries = prev })
	RawQueries = []storm.RawDecl{
		storm.ShardedSQL[struct{ N int32 }](`SELECT n FROM orders`),
		storm.ShardedSQLExec(`DELETE FROM orders`),
	}
	if got := plainRawStatements(); len(got) != 0 {
		t.Errorf("got %d plain statements, want none:\n%v", len(got), got)
	}
}

func TestPlainRawStatementsHandlesNoDeclarations(t *testing.T) {
	prev := RawQueries
	t.Cleanup(func() { RawQueries = prev })
	RawQueries = nil
	if got := plainRawStatements(); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

// shOrder is a sharded model for the end-to-end refusal below.
type shOrder struct {
	storm.Model
	TenantID storm.UUID
	Status   string
}

func (o *shOrder) Schema(t *storm.Table) { t.ShardKey(&o.TenantID) }

type shCount struct{ N int64 }

// End to end, through the command an adopter runs. The halves are tested
// above and in codegen: which declarations count as plain, and that a plain
// one naming a sharded table is refused. Only this proves that `storm
// generate` wires one into the other. The statement is valid SQL and PREPAREs
// fine, which is the point: nothing but this refusal stands between it and a
// query that reads one shard's rows as if they were all of them.
func TestGenerateRefusesPlainSQLOnAShardedTable(t *testing.T) {
	if os.Getenv("STORM_DSN") == "" {
		t.Skip("STORM_DSN unset: generate validates raw queries against a server first")
	}
	withModels(t, []any{&shOrder{}})
	prev := RawQueries
	RawQueries = []storm.RawDecl{
		storm.SQL[shCount](`SELECT count(*) AS n FROM sh_orders WHERE tenant_id = $1`),
	}
	t.Cleanup(func() { RawQueries = prev })

	dir := filepath.Join(moduleScratch(t, "clishard"), "store")
	err := run([]string{"generate", dir})
	if err == nil {
		t.Fatal("a plain storm.SQL on a sharded table was generated")
	}
	for _, want := range []string{"sh_orders", "ShardedSQL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}
