package pgxdrv

import (
	"encoding/binary"
	"testing"
)

// An array is rarely the last parameter of a statement. Once one is written,
// the next scalar — a LIMIT, a key — must fit in the buffer the array grew,
// not make append copy the whole array again to add eight bytes.
func TestAnArrayLeavesRoomForTheParametersAfterIt(t *testing.T) {
	ids := make([][16]byte, 2000)
	ints := make([]int64, 2000)
	texts := make([]string, 2000)
	for i := range texts {
		texts[i] = "key"
	}
	for name, encode := range map[string]func([]byte) ([]byte, error){
		"uuid[]": func(b []byte) ([]byte, error) { return uuidArrayPlan{}.Encode(ids, b) },
		"int8[]": func(b []byte) ([]byte, error) { return int8ArrayPlan{}.Encode(ints, b) },
		"text[]": func(b []byte) ([]byte, error) { return textArrayPlan{}.Encode(texts, b) },
	} {
		buf, err := encode(make([]byte, 0, 64))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		before := &buf[:cap(buf)][cap(buf)-1]
		for range 8 {
			buf = binary.BigEndian.AppendUint32(buf, 8)
			buf = binary.BigEndian.AppendUint64(buf, 1)
		}
		if after := &buf[:cap(buf)][cap(buf)-1]; cap(buf) == 0 || after != before {
			t.Errorf("%s: eight scalar parameters after the array reallocated the buffer", name)
		}
	}
}
