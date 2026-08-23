package pgxdrv_test

import (
	"bytes"
	"testing"

	"github.com/gsoultan/raorm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgtype"
)

// The fast encoder must produce the SAME BYTES pgx would. A parameter encoder
// that is fast and subtly wrong corrupts a query's meaning rather than failing
// it, and `= ANY` is on the hot path of every relation load.
func TestFastUUIDArray_MatchesPgxByteForByte(t *testing.T) {
	slow := pgtype.NewMap()
	fast := pgtype.NewMap()
	pgxdrv.RegisterFastArrays(fast)

	for _, n := range []int{0, 1, 2, 7, 500} {
		ids := make([][16]byte, n)
		for i := range ids {
			for b := range ids[i] {
				ids[i][b] = byte(i*31 + b)
			}
		}

		want, err := slow.Encode(pgtype.UUIDArrayOID, pgtype.BinaryFormatCode, ids, nil)
		if err != nil {
			t.Fatalf("n=%d: pgx could not encode: %v", n, err)
		}
		got, err := fast.Encode(pgtype.UUIDArrayOID, pgtype.BinaryFormatCode, ids, nil)
		if err != nil {
			t.Fatalf("n=%d: fast encoder failed: %v", n, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("n=%d: encodings differ\n got  %x\n want %x", n, got, want)
		}
	}
}

// Appending to a non-empty buffer must not clobber what is already there — pgx
// passes a buffer with the message header already written.
func TestFastUUIDArray_AppendsToAnExistingBuffer(t *testing.T) {
	m := pgtype.NewMap()
	pgxdrv.RegisterFastArrays(m)

	prefix := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	buf := make([]byte, len(prefix), 8) // deliberately too small to hold the array
	copy(buf, prefix)

	got, err := m.Encode(pgtype.UUIDArrayOID, pgtype.BinaryFormatCode, [][16]byte{{1}, {2}}, buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, prefix) {
		t.Errorf("the existing buffer contents were lost: %x", got)
	}
}

// A nil slice is SQL NULL, not an empty array. Encoding it as an empty array
// would turn `= ANY(NULL)` into `= ANY('{}')`, which matches nothing instead of
// being unknown — a silent change of meaning.
func TestFastUUIDArray_NilIsNull(t *testing.T) {
	m := pgtype.NewMap()
	pgxdrv.RegisterFastArrays(m)

	got, err := m.Encode(pgtype.UUIDArrayOID, pgtype.BinaryFormatCode, [][16]byte(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("a nil slice encoded to %x, want nil (SQL NULL)", got)
	}
}

// Everything the override does not handle must still work: other Go types, and
// the text format.
func TestFastUUIDArray_DelegatesEverythingElse(t *testing.T) {
	slow := pgtype.NewMap()
	fast := pgtype.NewMap()
	pgxdrv.RegisterFastArrays(fast)

	ids := []pgtype.UUID{{Bytes: [16]byte{9}, Valid: true}}
	want, err := slow.Encode(pgtype.UUIDArrayOID, pgtype.BinaryFormatCode, ids, nil)
	if err != nil {
		t.Skipf("pgx cannot encode %T either: %v", ids, err)
	}
	got, err := fast.Encode(pgtype.UUIDArrayOID, pgtype.BinaryFormatCode, ids, nil)
	if err != nil {
		t.Fatalf("delegation failed for %T: %v", ids, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("delegated encoding differs\n got  %x\n want %x", got, want)
	}
}
