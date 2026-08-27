package runtime_test

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

// arr builds a one-dimensional binary array; a size of -1 marks a NULL element.
func arr(elems ...[]byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, 1)
	b = binary.BigEndian.AppendUint32(b, 0)
	b = binary.BigEndian.AppendUint32(b, 25) // text
	b = binary.BigEndian.AppendUint32(b, uint32(len(elems)))
	b = binary.BigEndian.AppendUint32(b, 1)
	for _, e := range elems {
		if e == nil {
			b = binary.BigEndian.AppendUint32(b, ^uint32(0)) // -1: NULL
			continue
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(e)))
		b = append(b, e...)
	}
	return b
}

func TestArray_NullVersusEmpty(t *testing.T) {
	var sl runtime.Slab

	// SQL NULL: no bytes at all.
	got, err := runtime.TextArray(nil, &sl)
	if err != nil || got != nil {
		t.Errorf("NULL decoded to %v, %v — want nil with no error", got, err)
	}

	// '{}': a header saying zero dimensions.
	empty := binary.BigEndian.AppendUint32(nil, 0)
	empty = binary.BigEndian.AppendUint32(empty, 0)
	empty = binary.BigEndian.AppendUint32(empty, 25)
	got, err = runtime.TextArray(empty, &sl)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Error("'{}' decoded to nil, which is how SQL NULL reads — they are different facts")
	}
	if len(got) != 0 {
		t.Errorf("'{}' decoded to %v", got)
	}
}

func TestArray_Elements(t *testing.T) {
	var sl runtime.Slab
	got, err := runtime.TextArray(arr([]byte("a"), []byte(""), []byte("ccc")), &sl)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "a" || got[1] != "" || got[2] != "ccc" {
		t.Errorf("got %q", got)
	}
}

func TestArray_NullElementIsAnError(t *testing.T) {
	var sl runtime.Slab
	if _, err := runtime.TextArray(arr([]byte("a"), nil, []byte("c")), &sl); !errors.Is(err, runtime.ErrArrayNull) {
		t.Errorf("a NULL element returned %v, want ErrArrayNull", err)
	}
}

func TestArray_MultiDimensionalIsAnError(t *testing.T) {
	var sl runtime.Slab
	two := binary.BigEndian.AppendUint32(nil, 2)
	two = binary.BigEndian.AppendUint32(two, 0)
	two = binary.BigEndian.AppendUint32(two, 25)
	two = binary.BigEndian.AppendUint32(two, 1)
	two = binary.BigEndian.AppendUint32(two, 1)
	if _, err := runtime.TextArray(two, &sl); !errors.Is(err, runtime.ErrArrayDims) {
		t.Errorf("a 2-D array returned %v, want ErrArrayDims", err)
	}
}

// Truncated input must be an error, never a panic: a wire value is the one
// input the library does not control.
func TestArray_TruncatedIsAnError(t *testing.T) {
	var sl runtime.Slab
	full := arr([]byte("abc"))
	for n := 1; n < len(full); n++ {
		if _, err := runtime.TextArray(full[:n], &sl); err == nil {
			t.Errorf("a value truncated to %d bytes decoded without error", n)
		}
	}
}

func TestArray_UUIDRoundTrip(t *testing.T) {
	want := [][16]byte{{1, 2, 3}, {9}}
	b := runtime.EncodeArray(want, 2950, nil, func(v [16]byte, buf []byte) []byte {
		return append(buf, v[:]...)
	})
	got, err := runtime.UUIDArray(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
	if runtime.EncodeArray[[16]byte](nil, 2950, nil, nil) != nil {
		t.Error("a nil slice must encode as SQL NULL")
	}
}

func FuzzTextArray(f *testing.F) {
	f.Add(arr([]byte("a")))
	f.Add(arr())
	f.Add([]byte{0, 0, 0, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		var sl runtime.Slab
		// The only property: never panic. Any error is fine.
		_, _ = runtime.TextArray(b, &sl)
	})
}

// A header may not command an allocation the input cannot justify. The fuzzer
// found a 17-byte value whose count field read as ~800 million, and
// make([]string, 0, n) allocated gigabytes before the first element was read —
// an OOM kill, not a panic. The allocation must be bounded by the INPUT SIZE,
// never by a field inside it.
func TestArray_HugeClaimedCountIsAnErrorNotAnAllocation(t *testing.T) {
	var sl runtime.Slab
	b := binary.BigEndian.AppendUint32(nil, 1)  // ndim
	b = binary.BigEndian.AppendUint32(b, 0)     // hasnull
	b = binary.BigEndian.AppendUint32(b, 25)    // text
	b = binary.BigEndian.AppendUint32(b, 1<<30) // a billion elements...
	b = binary.BigEndian.AppendUint32(b, 1)     // ...in a 20-byte value
	if _, err := runtime.TextArray(b, &sl); err == nil {
		t.Fatal("a count the bytes cannot hold must be an error before it is an allocation")
	}
}
