package pgxdrv_test

import (
	"context"
	"os"
	"testing"

	"github.com/gsoultan/raorm/runtime"
	"github.com/jackc/pgx/v5"
)

// Generated scanners decode RawValues() as Postgres BINARY format. Under the
// simple protocol every value arrives as TEXT instead, and the decoders do
// not notice: `false` is the byte 'f' (0x66), which is not zero, so
// runtime.Bool reports TRUE. This test exists to prove that hazard is real
// rather than argued — it is the evidence behind
// docs/PRODUCTION-READINESS.md P0.1, and it must keep failing to be silent
// until the guard lands, at which point it becomes the guard's test.
func TestWireFormat_SimpleProtocolInvertsBool(t *testing.T) {
	dsn := os.Getenv("RAORM_DSN")
	if dsn == "" {
		t.Skip("RAORM_DSN not set")
	}
	ctx := context.Background()

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly what an adopter sets to survive PgBouncer transaction pooling.
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, `SELECT false AS flag, 42::int8 AS n`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	raw := rows.RawValues()
	gotBool := runtime.Bool(raw[0])
	t.Logf("simple protocol: bool false arrives as %q (% x) → runtime.Bool = %v",
		raw[0], raw[0], gotBool)
	t.Logf("simple protocol: int8 42 arrives as %q (%d bytes)", raw[1], len(raw[1]))

	if !gotBool {
		t.Skip("simple protocol returned a binary-compatible false — the hazard " +
			"depends on pgx's format choice; re-verify before acting on P0.1")
	}
	t.Log("CONFIRMED: SELECT false decodes as true — silent inversion, no error")

	// The same query under the default (extended, binary) protocol is correct,
	// which is what makes the failure invisible in every test that does not
	// change the exec mode.
	def, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer def.Close(ctx)
	rows2, err := def.Query(ctx, `SELECT false AS flag`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows2.Close()
	if !rows2.Next() {
		t.Fatal("no row")
	}
	if runtime.Bool(rows2.RawValues()[0]) {
		t.Fatal("binary format also inverted — that would be a decoder bug, not a format hazard")
	}
}
