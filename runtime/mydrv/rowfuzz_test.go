package mydrv

import (
	"bufio"
	"bytes"
	"errors"
	"testing"
)

// rowConn is a conn reading one binary-protocol row packet with the column
// types given.
func rowConn(types []byte, row []byte) *stmt {
	var wire []byte
	wire = append(wire, byte(len(row)), byte(len(row)>>8), byte(len(row)>>16), 1)
	wire = append(wire, row...)
	c := &conn{r: bufio.NewReader(bytes.NewReader(wire)), cols: make([][]byte, len(types))}
	return &stmt{types: types, c: c}
}

// A length-encoded value whose 8-byte length does not fit an int is a short
// row, not a panic. The binary row reader compared off+adv+int(n) with the
// packet length, and int(n) of 0xffffffffffffffff is -1: the comparison
// passed and the slice expression panicked, so one hostile or corrupt row
// packet took down the process. The text reader beside it already refused an
// n past the packet before converting.
func TestBinaryRowRefusesALengthPastInt(t *testing.T) {
	row := []byte{0x00, 0x00} // header byte, null bitmap for one column
	row = append(row, 0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	_, err := rowConn([]byte{typeVarString}, row).nextRow()
	if !errors.Is(err, errShortRow) {
		t.Fatalf("err = %v, want errShortRow", err)
	}
}

// FuzzBinaryRow: no row packet panics the reader, whatever the column types,
// and every column it hands back lies inside the packet.
func FuzzBinaryRow(f *testing.F) {
	f.Add([]byte{typeLongLong, typeVarString}, []byte{0x00, 0x00, 1, 2, 3, 4, 5, 6, 7, 8, 0x02, 'h', 'i'})
	f.Add([]byte{typeDateTime}, []byte{0x00, 0x00, 0x04, 0xe8, 0x07, 1, 2})
	f.Add([]byte{typeVarString}, []byte{0x00, 0x00, 0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{typeVarString, typeVarString}, []byte{0x00, 0x04})
	f.Fuzz(func(t *testing.T, types, row []byte) {
		if len(types) == 0 || len(types) > 64 || len(row) > 1<<16 {
			return
		}
		s := rowConn(types, row)
		cols, err := s.nextRow()
		if err != nil {
			return
		}
		for i, c := range cols {
			if len(c) > len(row) {
				t.Fatalf("column %d is %d bytes from a %d-byte packet", i, len(c), len(row))
			}
		}
	})
}
