package bsondec

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/gsoultan/storm/runtime"
)

// Array decodes a BSON array whose elements dec reads, in order.
//
// A null element is an error rather than a zero: the Go slice the column
// becomes cannot say NULL, and an element that silently turned into "" or 0
// would be a value nobody stored.
func Array[T any](b []byte, dec func([]byte) (T, error)) ([]T, error) {
	if len(b) < 1 || b[0] != TArray {
		return nil, shape("an array", b)
	}
	out := []T{}
	err := Elements(b[1:], func(t byte, _ string, v []byte) error {
		if t == TNull {
			return fmt.Errorf("%w: the array holds a null element, which a Go slice cannot carry", runtime.ErrShape)
		}
		el := make([]byte, 0, 1+len(v))
		el = append(append(el, t), v...)
		x, err := dec(el)
		if err != nil {
			return err
		}
		out = append(out, x)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

var errDoc = errors.New("bsondec: malformed BSON document")

// Elements walks a BSON document or array body — its length prefix through
// its terminating zero — calling fn with each element's type, name and value
// bytes.
func Elements(doc []byte, fn func(t byte, name string, v []byte) error) error {
	if len(doc) < 5 {
		return fmt.Errorf("%w: %w", runtime.ErrShape, errDoc)
	}
	n := int(binary.LittleEndian.Uint32(doc))
	if n != len(doc) || doc[n-1] != 0 {
		return fmt.Errorf("%w: %w", runtime.ErrShape, errDoc)
	}
	i := 4
	for i < n-1 {
		t := doc[i]
		i++
		j := i
		for j < n && doc[j] != 0 {
			j++
		}
		if j >= n {
			return fmt.Errorf("%w: %w", runtime.ErrShape, errDoc)
		}
		name := string(doc[i:j])
		i = j + 1
		sz := Size(t, doc[i:])
		if sz < 0 || i+sz > n-1 {
			return fmt.Errorf("%w: %w", runtime.ErrShape, errDoc)
		}
		if err := fn(t, name, doc[i:i+sz]); err != nil {
			return err
		}
		i += sz
	}
	return nil
}

// Size is the length of a value of BSON type t at the start of b, or -1 for a
// type or length it cannot read.
func Size(t byte, b []byte) int {
	need := func(k int) int {
		if len(b) < k {
			return -1
		}
		return k
	}
	prefixed := func(extra int) int {
		if len(b) < 4 {
			return -1
		}
		k := int(int32(binary.LittleEndian.Uint32(b))) + extra
		if k < 4 || k > len(b) {
			return -1
		}
		return k
	}
	switch t {
	case TDouble, TDateTime, TInt64, 0x11: // 0x11 is a timestamp
		return need(8)
	case TInt32:
		return need(4)
	case TDecimal:
		return need(16)
	case TBool:
		return need(1)
	case TNull, 0x06, 0xFF, 0x7F: // null, undefined, min key, max key
		return 0
	case 0x07: // ObjectId
		return need(12)
	case TString, 0x0D, 0x0E: // string, JavaScript, symbol
		return prefixed(4)
	case TDocument, TArray, 0x0F: // document, array, code with scope
		return prefixed(0)
	case TBinary:
		return prefixed(5)
	case 0x0B: // regex: two C strings
		k := 0
		for c := 0; c < 2; c++ {
			for k < len(b) && b[k] != 0 {
				k++
			}
			if k >= len(b) {
				return -1
			}
			k++
		}
		return k
	}
	return -1
}
