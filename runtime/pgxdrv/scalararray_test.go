package pgxdrv_test

import (
	"context"
	"math"
	"testing"

	"github.com/gsoultan/raorm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5"
)

// A hand-written wire encoder that is subtly wrong does not fail — it stores
// or matches the wrong values, which is worse than a crash and is the entire
// risk of writing one. So every case round-trips through a real server and is
// compared against what PostgreSQL itself says it received, with the fast
// codec installed on one connection and pgx's generic codec on the other:
// agreement between the two is the property, not agreement with my
// expectations.
func TestFastArrays_RoundTripAgreesWithPgx(t *testing.T) {
	ctx := context.Background()
	d := dsn(t)

	fast, err := pgx.Connect(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Close(ctx)
	pgxdrv.RegisterFastArrays(fast.TypeMap())

	plain, err := pgx.Connect(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close(ctx)

	t.Run("int8", func(t *testing.T) {
		for _, in := range [][]int64{
			{},
			{0},
			{42, -1, 7},
			{math.MinInt64, math.MaxInt64},
			func() []int64 { // a realistic loader key set
				v := make([]int64, 500)
				for i := range v {
					v[i] = int64(i) * 7
				}
				return v
			}(),
		} {
			var viaFast, viaPlain []int64
			if err := fast.QueryRow(ctx, `SELECT $1::int8[]`, in).Scan(&viaFast); err != nil {
				t.Fatalf("fast codec: %v", err)
			}
			if err := plain.QueryRow(ctx, `SELECT $1::int8[]`, in).Scan(&viaPlain); err != nil {
				t.Fatalf("generic codec: %v", err)
			}
			if len(viaFast) != len(in) {
				t.Fatalf("round-tripped %d values, sent %d", len(viaFast), len(in))
			}
			for i := range in {
				if viaFast[i] != in[i] || viaFast[i] != viaPlain[i] {
					t.Fatalf("element %d: sent %d, fast %d, generic %d",
						i, in[i], viaFast[i], viaPlain[i])
				}
			}
		}
	})

	t.Run("text", func(t *testing.T) {
		// The binary format carries lengths rather than delimiters, which is
		// exactly why these inputs are safe — and why they must be proven so
		// rather than assumed.
		for _, in := range [][]string{
			{},
			{""},
			{"plain"},
			{"a,b", "{braced}", `quo"ted`, `back\slash`, "new\nline", "tab\there"},
			{"unicode ✓ ünïcødé 日本語", "emoji 🎯"},
			{"NULL", "null", "\\N"}, // the strings, not the value
		} {
			var viaFast, viaPlain []string
			if err := fast.QueryRow(ctx, `SELECT $1::text[]`, in).Scan(&viaFast); err != nil {
				t.Fatalf("fast codec on %q: %v", in, err)
			}
			if err := plain.QueryRow(ctx, `SELECT $1::text[]`, in).Scan(&viaPlain); err != nil {
				t.Fatalf("generic codec on %q: %v", in, err)
			}
			if len(viaFast) != len(in) {
				t.Fatalf("round-tripped %d values, sent %d", len(viaFast), len(in))
			}
			for i := range in {
				if viaFast[i] != in[i] || viaFast[i] != viaPlain[i] {
					t.Fatalf("element %d: sent %q, fast %q, generic %q",
						i, in[i], viaFast[i], viaPlain[i])
				}
			}
		}
	})

	// nil is SQL NULL, not an empty array — the same distinction the decoders
	// keep, and a place an encoder could quietly collapse two different facts.
	t.Run("nil is NULL, empty is '{}'", func(t *testing.T) {
		var isNull bool
		if err := fast.QueryRow(ctx, `SELECT $1::int8[] IS NULL`, []int64(nil)).Scan(&isNull); err != nil {
			t.Fatal(err)
		}
		if !isNull {
			t.Fatal("a nil []int64 must bind as NULL")
		}
		if err := fast.QueryRow(ctx, `SELECT $1::text[] IS NULL`, []string(nil)).Scan(&isNull); err != nil {
			t.Fatal(err)
		}
		if !isNull {
			t.Fatal("a nil []string must bind as NULL")
		}
		var n int
		if err := fast.QueryRow(ctx, `SELECT cardinality($1::int8[])`, []int64{}).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("an empty []int64 bound as cardinality %d", n)
		}
	})

	// The reason any of this exists: matching a key set server-side.
	t.Run("= ANY matches what it should", func(t *testing.T) {
		var got int64
		if err := fast.QueryRow(ctx,
			`SELECT count(*) FROM (SELECT unnest(ARRAY[1,2,3]::int8[]) AS v) s WHERE s.v = ANY($1::int8[])`,
			[]int64{2, 3, 99}).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != 2 {
			t.Fatalf("= ANY matched %d rows, want 2", got)
		}
	})
}
