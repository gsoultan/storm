package pgxdrv_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
)

// What storm writes must be what PostgreSQL reads.
//
// Byte-equality with pgx is the wrong assertion and was tried first: the binary
// numeric format has freedom storm and pgx spend differently. pgx keeps leading
// zero groups where storm is compact, and sets a weight on a value with no
// digits at all, where weight means nothing. Both are the same number. The only
// question worth asking is what the server makes of the bytes, so this asks the
// server.
//
// The values below are the ones a Decimal holds but the encoder used to lose.
// Padding a scale up to a group boundary was done by multiplying the unscaled
// value by up to 1,000, which overflows int64 past about 9.2e15 — u went
// negative, the digit loop never ran, and the zero encoding went out. At scale
// 9 that starts at roughly 9.2 million, so a rate or a token amount in a
// numeric(30,9) column was silently stored as 0.000000000, on insert and in
// predicates alike.
func TestNumericWireIsWhatPostgresReads(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	pgxdrv.RegisterFastArrays(conn.TypeMap()) // installs the decimal codec too

	for _, in := range []string{
		"0", "1", "-1", "0.01", "-0.01", "1000000", "0.000000001",
		"123456789.987654321", "-123456789.987654321",
		"9223372.036854775", "12345678.123456789",
		"922337203685477.5", "92233720368.5477580",
		"-0.000000001", "9999999999999999.9",
	} {
		d, err := runtime.ParseDecimal(in)
		if err != nil {
			t.Fatalf("ParseDecimal(%q): %v", in, err)
		}

		// The scalar path: what an INSERT or a predicate binds.
		var scalar string
		if err := conn.QueryRow(ctx, "SELECT $1::numeric::text", d).Scan(&scalar); err != nil {
			t.Errorf("%q: scalar: %v", in, err)
			continue
		}
		if !sameNumber(scalar, in) {
			t.Errorf("scalar %q was stored as %q", in, scalar)
		}

		// The array path: what `= ANY($1)` binds.
		var arr string
		if err := conn.QueryRow(ctx, "SELECT ($1::numeric[])[1]::text",
			[]runtime.Decimal{d}).Scan(&arr); err != nil {
			t.Errorf("%q: array: %v", in, err)
			continue
		}
		if !sameNumber(arr, in) {
			t.Errorf("array %q was stored as %q", in, arr)
		}
	}
}

// sameNumber compares two numeric texts by value, not spelling: PostgreSQL
// renders the scale it was given, and "1" and "1.0" are the same number.
func sameNumber(a, b string) bool {
	norm := func(s string) string {
		neg := false
		if len(s) > 0 && (s[0] == '-' || s[0] == '+') {
			neg = s[0] == '-'
			s = s[1:]
		}
		if i := indexByte(s, '.'); i >= 0 {
			for len(s) > 0 && s[len(s)-1] == '0' {
				s = s[:len(s)-1]
			}
			if len(s) > 0 && s[len(s)-1] == '.' {
				s = s[:len(s)-1]
			}
		}
		for len(s) > 1 && s[0] == '0' && s[1] != '.' {
			s = s[1:]
		}
		if s == "0" || s == "" {
			return "0"
		}
		if neg {
			return "-" + s
		}
		return s
	}
	return norm(a) == norm(b)
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
