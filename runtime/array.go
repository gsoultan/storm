package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Array decoding.
//
// PostgreSQL's binary array is a header — dimension count, a null flag, the
// element OID — then one length-prefixed element after another. Multi-
// dimensional arrays are legal and a []T cannot hold one, so they are an error
// rather than a flattened surprise.
//
// # NULL elements
//
// `{1, NULL, 3}` is a legal text[] and a []string cannot represent it. Decoding
// the NULL as "" would make an absent value and an empty one the same thing —
// the exact conflation Null[T] exists to prevent everywhere else. So it is an
// error, and the message says what to do about it.
//
// # Allocation
//
// One allocation per array, sized exactly from the header. A variable-length
// value cannot avoid that; what it can avoid is growing a slice element by
// element, which is what a naive decoder does.

// ErrArrayNull means an array contained a NULL element, which a []T cannot
// represent.
var ErrArrayNull = errors.New(
	"raorm: array contains a NULL element, which a Go slice cannot represent — " +
		"filter them out in SQL with array_remove(col, NULL), or read the column as jsonb")

// ErrArrayDims means an array had more than one dimension.
var ErrArrayDims = errors.New(
	"raorm: multi-dimensional array cannot be read into a Go slice")

// ErrArrayTextFormat means the server sent the array as text and raorm
// decodes the binary format.
//
// The case that reaches real schemas is an array of a USER-DEFINED type —
// `my_enum[]` — because pgx has no binary codec for one and falls back to
// text. A plain text[] is unaffected: pgx sends it as binary. The scalar enum
// is unaffected too, since its label is the same bytes either way, which is
// why the executor's format guard lets user-defined types through.
//
// Reading the text format here was considered and rejected: it means a second
// array parser with its own quoting and escaping rules to keep faithful to
// the first, for a case with two better answers — declare the column
// `text[]`, or cast it in the query (`col::text[]`).
var ErrArrayTextFormat = errors.New(
	"raorm: array arrived in TEXT format and raorm decodes binary — an array of a " +
		"user-defined type (enum[]) is sent as text by pgx; declare the column text[] " +
		"or cast it in the query with col::text[]; see docs/DEPLOYMENT.md")

// arrayHeader reads the fixed prefix and returns the element count and the
// offset of the first element.
func arrayHeader(b []byte) (n, off int, err error) {
	if len(b) == 0 {
		return 0, 0, nil // SQL NULL
	}
	// A binary array begins with a big-endian int32 dimension count, so its
	// first byte is zero for every plausible value; the text format begins
	// with '{'. Naming the real cause here is the difference between an
	// actionable error and "it has 2069982320", which is what the dimension
	// check reported when it read four bytes of "{alp" as a number.
	if b[0] == '{' {
		return 0, 0, ErrArrayTextFormat
	}
	if len(b) < 12 {
		return 0, 0, errors.New("raorm: array wire value is too short")
	}
	ndim := int(int32(binary.BigEndian.Uint32(b[0:4])))
	switch {
	case ndim == 0:
		return 0, 12, nil // '{}' — empty, not NULL
	case ndim != 1:
		return 0, 0, fmt.Errorf("%w: it has %d", ErrArrayDims, ndim)
	}
	if len(b) < 20 {
		return 0, 0, errors.New("raorm: array wire value is truncated")
	}
	n = int(int32(binary.BigEndian.Uint32(b[12:16])))
	if n < 0 {
		return 0, 0, errors.New("raorm: array reports a negative length")
	}
	// Bound the claimed count by what the bytes could possibly hold: every
	// element costs at least its 4-byte length prefix. Without this, a corrupt
	// header saying "800 million elements" makes the decoder allocate
	// gigabytes BEFORE reading a single element — the fuzzer found it as an
	// out-of-memory kill rather than a panic, which is exactly how it would
	// present in production. The allocation must be bounded by the input, not
	// by a field inside it.
	if n > (len(b)-20)/4 {
		return 0, 0, errors.New("raorm: array claims more elements than the value could hold")
	}
	return n, 20, nil
}

// Array decodes a one-dimensional array using dec for each element.
//
// A nil result means SQL NULL; an empty non-nil slice means '{}'. Those are
// different facts and a caller checking `len(x) == 0` conflates them, so the
// distinction is preserved rather than smoothed over.
func Array[T any](b []byte, dec func([]byte) T) ([]T, error) {
	return ArrayErr(b, func(e []byte) (T, error) { return dec(e), nil })
}

// ArrayErr is Array for element decoders that can fail — numeric is one, and
// a numeric[] whose third element is corrupt must say so rather than decode
// to a zero.
//
// It is the single implementation, with Array wrapping it, because the bounds
// arithmetic below is the part a fuzzer already found a hole in once: a count
// field claiming 800 million elements made the decoder allocate gigabytes
// before reading a byte. Two copies of that logic would eventually disagree,
// and the cheaper copy is not worth the risk.
func ArrayErr[T any](b []byte, dec func([]byte) (T, error)) ([]T, error) {
	n, off, err := arrayHeader(b)
	if err != nil || off == 0 {
		return nil, err
	}
	out := make([]T, 0, n)
	for i := 0; i < n; i++ {
		if off+4 > len(b) {
			return nil, errors.New("raorm: array element is truncated")
		}
		size := int(int32(binary.BigEndian.Uint32(b[off : off+4])))
		off += 4
		if size < 0 {
			return nil, ErrArrayNull
		}
		if off+size > len(b) {
			return nil, errors.New("raorm: array element runs past the value")
		}
		v, err := dec(b[off : off+size])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		off += size
	}
	return out, nil
}

// TextArray decodes a text[] into the arena, so a row of strings costs one
// allocation for the slice rather than one per element.
func TextArray(b []byte, s *Slab) ([]string, error) {
	// ArrayErr directly, not through Array: going via the wrapper costs a
	// SECOND call per element (wrapper then dec), which measured 45% on a
	// 500-element decode. One indirection is the price of a generic
	// decoder; two is an accident.
	return ArrayErr(b, func(e []byte) (string, error) { return s.Str(e), nil })
}

// UUIDArray decodes a uuid[].
func UUIDArray(b []byte) ([][16]byte, error) {
	return ArrayErr(b, func(e []byte) ([16]byte, error) {
		var v [16]byte
		copy(v[:], e)
		return v, nil
	})
}

// DecimalArray decodes a numeric[]. Element decoding is fallible — a numeric
// past the 18 significant digits a Decimal holds is an error rather than a
// wrong number, and that has to survive being inside an array.
func DecimalArray(b []byte) ([]Decimal, error) {
	return ArrayErr(b, DecodeNumeric)
}

// EncodeArray writes a one-dimensional array. enc appends one element's bytes.
//
// Used for binding an array parameter. NULL elements are not expressible on the
// way out either, which is consistent: a []T has none to write.
func EncodeArray[T any](vs []T, elemOID uint32, buf []byte, enc func(T, []byte) []byte) []byte {
	if vs == nil {
		return nil
	}
	buf = binary.BigEndian.AppendUint32(buf, 1)               // ndim
	buf = binary.BigEndian.AppendUint32(buf, 0)               // hasnull
	buf = binary.BigEndian.AppendUint32(buf, elemOID)         //
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(vs))) // length
	buf = binary.BigEndian.AppendUint32(buf, 1)               // lower bound
	for _, v := range vs {
		at := len(buf)
		buf = binary.BigEndian.AppendUint32(buf, 0) // placeholder length
		buf = enc(v, buf)
		binary.BigEndian.PutUint32(buf[at:at+4], uint32(len(buf)-at-4))
	}
	return buf
}
