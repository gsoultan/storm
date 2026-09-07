package storm_test

import (
	"context"
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/jackc/pgx/v5"
)

// Compiling is not decoding. This release's own lesson twice over, so the new
// array codec reads bytes PostgreSQL actually sent, on a binary connection,
// rather than bytes a test constructed to match the decoder.
func TestInt4ArrayDecodesRealWireBytes(t *testing.T) {
	c := connect(t)
	ctx := context.Background()

	for _, tc := range []struct {
		sql  string
		want []int32
	}{
		{`'{22,80,443}'::int4[]`, []int32{22, 80, 443}},
		{`'{}'::int4[]`, []int32{}},
		{`'{-1,0,2147483647,-2147483648}'::int4[]`, []int32{-1, 0, 2147483647, -2147483648}},
	} {
		rows, err := c.Query(ctx, `SELECT `+tc.sql, pgx.QueryResultFormats{pgx.BinaryFormatCode})
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		if !rows.Next() {
			rows.Close()
			t.Fatalf("%s: no row", tc.sql)
		}
		raw := rows.RawValues()[0]
		got, err := runtime.Int4Array(raw)
		rows.Close()
		if err != nil {
			t.Fatalf("%s: Int4Array: %v", tc.sql, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.sql, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: got %v, want %v", tc.sql, got, tc.want)
			}
		}
	}

	// A NULL element is a distinct fact from an empty array, and the array
	// decoders refuse it rather than inventing a zero — the same rule the
	// other element types follow.
	rows, err := c.Query(ctx, `SELECT '{1,NULL,3}'::int4[]`, pgx.QueryResultFormats{pgx.BinaryFormatCode})
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		rows.Close()
		t.Fatal("no row")
	}
	raw := rows.RawValues()[0]
	_, err = runtime.Int4Array(raw)
	rows.Close()
	if err == nil {
		t.Error("a NULL element decoded without error; it must not be silently zero")
	}
}
