package runtime_test

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

// FuzzFallibleDecoders: no value a PostgreSQL server sends panics one of the
// decoders that read a length or a count out of the value itself — the ones
// the generated scanner's width guard cannot protect, because their width is
// not fixed. (The fixed-width decoders are behind that guard; see ShapeErr.)
func FuzzFallibleDecoders(f *testing.F) {
	for _, s := range [][]byte{
		{}, {0, 1, 0, 0, 0, 0, 0, 0}, {0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0x14, 0, 0, 0, 1, 0, 0, 0, 1},
		{2, 32, 0, 4, 10, 0, 0, 1}, {3, 128, 0, 16, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
		{0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 8, 0, 0, 0, 0, 0, 0, 0, 0},
		{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var sl runtime.Slab
		_ = runtime.Bytes(b)
		_ = runtime.NullText(b, &sl)
		_ = runtime.JSONB(b, &sl)
		_ = runtime.NullJSON(b, &sl)
		_, _ = runtime.NumericErr(b)
		_, _ = runtime.NullNumeric(b)
		_, _ = runtime.IntervalErr(b)
		_, _ = runtime.NullInterval(b)
		_, _ = runtime.TimeOfDayErr(b)
		_, _ = runtime.NullTimeOfDay(b)
		_, _ = runtime.DecodeTstzRange(b)
		_, _ = runtime.NullTstzRange(b)
		_, _ = runtime.InetErr(b)
		_, _ = runtime.NullInet(b)
		_, _ = runtime.TextArray(b, &sl)
		_, _ = runtime.UUIDArray(b)
		_, _ = runtime.DecimalArray(b)
		_, _ = runtime.Int8Array(b)
		_, _ = runtime.Int4Array(b)
	})
}

// An int4[] where the model says int8[] — or any element whose width is not
// its type's — is ErrShape. The element decoders read a fixed width blindly,
// so before the check the drifted array PANICKED inside the request: the
// scalar scanner's width guard cannot see one level down into an array.
func TestFixedWidthArrayElementsAreSized(t *testing.T) {
	arr := func(elemWidth int) []byte {
		var b []byte
		for _, v := range []uint32{1, 0, 23, 1, 1} { // ndim, hasnull, oid, length, lower bound
			b = binary.BigEndian.AppendUint32(b, v)
		}
		b = binary.BigEndian.AppendUint32(b, uint32(elemWidth))
		return append(b, make([]byte, elemWidth)...)
	}
	if _, err := runtime.Int8Array(arr(4)); !errors.Is(err, runtime.ErrShape) {
		t.Errorf("int8[] of 4-byte elements: %v", err)
	}
	if _, err := runtime.Int4Array(arr(8)); !errors.Is(err, runtime.ErrShape) {
		t.Errorf("int4[] of 8-byte elements: %v", err)
	}
	if _, err := runtime.UUIDArray(arr(4)); !errors.Is(err, runtime.ErrShape) {
		t.Errorf("uuid[] of 4-byte elements: %v", err)
	}
	if v, err := runtime.Int8Array(arr(8)); err != nil || len(v) != 1 {
		t.Errorf("a well-formed int8[]: %v, %v", v, err)
	}
}
