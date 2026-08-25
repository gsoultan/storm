package pgxdrv_test

import (
	"bytes"
	"testing"

	"github.com/gsoultan/raorm/runtime"
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

// A Decimal must reach the database as a numeric, not as text that Postgres
// coerces by context — a prepared statement resolves that context before the
// value exists.
func TestFastDecimal_EncodesAsNumeric(t *testing.T) {
	m := pgtype.NewMap()
	pgxdrv.RegisterFastArrays(m)

	for _, s := range []string{"0", "1", "-1", "0.01", "1234.5678", "-1234.5678"} {
		d, err := runtime.ParseDecimal(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := m.Encode(pgtype.NumericOID, pgtype.BinaryFormatCode, d, nil)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		back, err := runtime.DecodeNumeric(got)
		if err != nil {
			t.Fatalf("%q: decoding what we encoded: %v", s, err)
		}
		if back.String() != s {
			t.Errorf("%q encoded and decoded back to %q", s, back.String())
		}
	}

	// A nil pointer is SQL NULL, not zero.
	got, err := m.Encode(pgtype.NumericOID, pgtype.BinaryFormatCode, (*runtime.Decimal)(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("a nil *Decimal encoded to %x, want nil (SQL NULL)", got)
	}
}

// The interval and decimal bridges: raorm's types encoded by the registered
// codecs, byte-compatible with what the decoders read back, nil pointers as
// SQL NULL, and everything else delegated.
func TestFastCodecs_IntervalAndDecimal(t *testing.T) {
	m := pgtype.NewMap()
	pgxdrv.RegisterFastArrays(m)

	iv := runtime.Interval{Months: 2, Days: 3, Micros: 4}
	b, err := m.Encode(pgtype.IntervalOID, pgtype.BinaryFormatCode, iv, nil)
	if err != nil {
		t.Fatal(err)
	}
	back, err := runtime.IntervalErr(b)
	if err != nil || back != iv {
		t.Errorf("interval round-trip = %+v, %v", back, err)
	}
	if b, err = m.Encode(pgtype.IntervalOID, pgtype.BinaryFormatCode, &iv, nil); err != nil {
		t.Fatal(err)
	}
	if back, _ = runtime.IntervalErr(b); back != iv {
		t.Errorf("*interval round-trip = %+v", back)
	}
	if b, err = m.Encode(pgtype.IntervalOID, pgtype.BinaryFormatCode, (*runtime.Interval)(nil), nil); err != nil || b != nil {
		t.Errorf("nil *Interval must encode as SQL NULL, got %x, %v", b, err)
	}
	// Delegation: pgtype's own Interval still encodes through the wrapped codec.
	if _, err := m.Encode(pgtype.IntervalOID, pgtype.BinaryFormatCode,
		pgtype.Interval{Microseconds: 5, Valid: true}, nil); err != nil {
		t.Errorf("delegation broke: %v", err)
	}

	d := runtime.Decimal{Unscaled: 12345, Scale: 2}
	b, err = m.Encode(pgtype.NumericOID, pgtype.BinaryFormatCode, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if back, err := runtime.DecodeNumeric(b); err != nil || back != d {
		t.Errorf("decimal round-trip = %+v, %v", back, err)
	}
	if b, err = m.Encode(pgtype.NumericOID, pgtype.BinaryFormatCode, &d, nil); err != nil {
		t.Fatal(err)
	}
	if back, _ := runtime.DecodeNumeric(b); back != d {
		t.Errorf("*decimal round-trip = %+v", back)
	}
	if b, err = m.Encode(pgtype.NumericOID, pgtype.BinaryFormatCode, (*runtime.Decimal)(nil), nil); err != nil || b != nil {
		t.Errorf("nil *Decimal must encode as SQL NULL, got %x, %v", b, err)
	}
}
