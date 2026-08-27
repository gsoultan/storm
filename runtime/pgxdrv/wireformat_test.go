package pgxdrv_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func dsn(t *testing.T) string {
	t.Helper()
	d := os.Getenv("STORM_DSN")
	if d == "" {
		t.Skip("STORM_DSN not set")
	}
	return d
}

// The hazard this guards, stated as the measurement that found it: generated
// scanners decode RAW wire bytes as binary, and under pgx's simple protocol
// every value arrives as text. `false` is the byte 'f' (0x66), which is not
// zero, so runtime.Bool reports TRUE — silently. This test asserts the
// hazard is REAL at the decoder and REFUSED at the executor; if the first
// half ever stops holding, the guard can be reconsidered, and until then it
// cannot be argued away.
func TestWireFormat_TextBoolWouldInvert(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(dsn(t))
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

	r, err := conn.Query(ctx, `SELECT false AS flag`)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if !r.Next() {
		t.Fatal("no row")
	}
	raw := r.RawValues()[0]
	if !runtime.Bool(raw) {
		t.Skipf("text false %q no longer decodes as true — re-derive the guard", raw)
	}
	t.Logf("decoder hazard confirmed: text false %q → runtime.Bool = true", raw)
}

// The executor refuses that result rather than handing back inverted values.
func TestWireFormat_ExecutorRefusesTextResults(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	// NewPoolConfig would refuse this outright (asserted below), so build the
	// pool the way an application with its own configuration would.
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ex := pgxdrv.Pool{P: pool}
	_, err = ex.Query(ctx, `SELECT false AS flag`, nil)
	if err == nil {
		t.Fatal("a text-format result must be refused, not decoded")
	}
	for _, want := range []string{`column 1 "flag"`, "text format", "PgBouncer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}

	// Batch takes the same path.
	berr := ex.Batch(ctx, []runtime.BatchOp{{SQL: `SELECT false AS flag`, WantRows: true}},
		func(_ int, _ runtime.Rows, _ int64, err error) error { return err })
	if berr == nil {
		t.Fatal("batch must refuse text-format results too")
	}
}

// Construction fails early for the two modes that send everything as text,
// so the common case never reaches a request.
func TestWireFormat_NewPoolRefusesTextModes(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []pgx.QueryExecMode{
		pgx.QueryExecModeSimpleProtocol,
		pgx.QueryExecModeExec,
	} {
		cfg, err := pgxpool.ParseConfig(dsn(t))
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.DefaultQueryExecMode = mode
		p, err := pgxdrv.NewPoolConfig(ctx, cfg)
		if err == nil {
			p.Close()
			t.Fatalf("%v must be refused at construction", mode)
		}
		if !strings.Contains(err.Error(), "invert") {
			t.Errorf("%v: error should say what goes wrong, got: %v", mode, err)
		}
	}
}

// The formats a working connection actually uses must keep passing — this is
// the half that would break if the check were written as "everything must be
// binary". Measured: pgx sends text for text/varchar and for jsonb, binary
// for bool, int8, uuid and bytea.
func TestWireFormat_DefaultModePasses(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxdrv.NewPool(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ex := pgxdrv.Pool{P: pool}

	rs, err := ex.Query(ctx, `
		SELECT false AS b, 42::int8 AS n, 'x'::text AS s, 'y'::varchar AS v,
		       '{"a":1}'::jsonb AS j, '\x0102'::bytea AS by,
		       gen_random_uuid() AS u, now() AS t`, nil)
	if err != nil {
		t.Fatalf("a default-mode result must be accepted: %v", err)
	}
	defer rs.Close()
	if !rs.Next() {
		t.Fatal("no row")
	}
	raw := rs.RawValues()
	if runtime.Bool(raw[0]) {
		t.Fatal("binary false decoded as true — that would be a decoder bug, not a format hazard")
	}
	if got := runtime.Int8(raw[1]); got != 42 {
		t.Fatalf("int8 = %d, want 42", got)
	}
	if got := string(raw[2]); got != "x" {
		t.Fatalf("text = %q", got)
	}
}
