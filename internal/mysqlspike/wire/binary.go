package main

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Prepared statements and the BINARY result set.
//
// This is the piece that makes runtime/mydec mean anything. COM_QUERY returns
// ASCII — "1000000" for a bigint — and mydec decodes MySQL's BINARY format:
// little-endian integers, component-wise temporals (ADR-0007). Without
// COM_STMT_PREPARE/EXECUTE the second decoder family the seam exists for has
// nothing to decode.

const (
	comStmtPrepare = 0x16
	comStmtExecute = 0x17
	comStmtClose   = 0x19
)

type stmt struct {
	c       *conn
	id      uint32
	nParams uint16
	nCols   uint16
}

// prepare issues COM_STMT_PREPARE and reads the statement's shape.
func (c *conn) prepare(sql string) (*stmt, error) {
	c.seq = 0
	body := make([]byte, 0, len(sql)+1)
	body = append(body, comStmtPrepare)
	body = append(body, sql...)
	if err := c.writePacket(body); err != nil {
		return nil, err
	}
	p, err := c.readPacket()
	if err != nil {
		return nil, err
	}
	if p[0] == 0xff {
		return nil, errors.New(string(p[9:]))
	}
	s := &stmt{
		c:       c,
		id:      binary.LittleEndian.Uint32(p[1:]),
		nCols:   binary.LittleEndian.Uint16(p[5:]),
		nParams: binary.LittleEndian.Uint16(p[7:]),
	}
	// Parameter definitions, then their EOF; column definitions, then theirs.
	// Each block's EOF is present because CLIENT_DEPRECATE_EOF is not set —
	// the same packet whose absence made the text path return zero rows.
	for _, n := range []uint16{s.nParams, s.nCols} {
		if n == 0 {
			continue
		}
		for i := uint16(0); i < n; i++ {
			if _, err := c.readPacket(); err != nil {
				return nil, err
			}
		}
		if _, err := c.readPacket(); err != nil { // EOF
			return nil, err
		}
	}
	return s, nil
}

func (s *stmt) close() error {
	s.c.seq = 0
	b := make([]byte, 5)
	b[0] = comStmtClose
	binary.LittleEndian.PutUint32(b[1:], s.id)
	return s.c.writePacket(b)
}

// MySQL column type codes, for the ones this spike binds and reads.
const (
	typeTiny      = 0x01
	typeShort     = 0x02
	typeLong      = 0x03
	typeLongLong  = 0x08
	typeDouble    = 0x05
	typeVarchar   = 0x0f
	typeString    = 0xfe
	typeVarString = 0xfd
	typeBlob      = 0xfc
	typeDateTime  = 0x0c
	typeDate      = 0x0a
	typeTime      = 0x0b
	typeNewDec    = 0xf6
)

// exec runs COM_STMT_EXECUTE and calls fn per row with RAW BINARY column bytes.
//
// args are bound as int64 or string only — enough to prove the path. A driver
// needs the full type table.
func (s *stmt) exec(args []any, fn func(cols [][]byte) error) error {
	s.c.seq = 0
	b := make([]byte, 0, 64)
	b = append(b, comStmtExecute)
	b = binary.LittleEndian.AppendUint32(b, s.id)
	b = append(b, 0)                           // no cursor
	b = binary.LittleEndian.AppendUint32(b, 1) // iteration count
	if len(args) > 0 {
		nullMap := make([]byte, (len(args)+7)/8)
		b = append(b, nullMap...)
		b = append(b, 1) // new params bound
		for _, a := range args {
			switch a.(type) {
			case int64, int:
				b = append(b, typeLongLong, 0)
			default:
				b = append(b, typeVarString, 0)
			}
		}
		for _, a := range args {
			switch v := a.(type) {
			case int64:
				b = binary.LittleEndian.AppendUint64(b, uint64(v))
			case int:
				b = binary.LittleEndian.AppendUint64(b, uint64(v))
			case string:
				b = appendLenEnc(b, []byte(v))
			default:
				return fmt.Errorf("spike binds int64 and string only, got %T", a)
			}
		}
	}
	if err := s.c.writePacket(b); err != nil {
		return err
	}

	p, err := s.c.readPacket()
	if err != nil {
		return err
	}
	if p[0] == 0xff {
		return errors.New(string(p[9:]))
	}
	if p[0] == 0x00 {
		return nil // OK: no result set
	}
	nCols, _, _ := lenEncInt(p)
	types := make([]byte, nCols)
	for i := uint64(0); i < nCols; i++ {
		def, err := s.c.readPacket()
		if err != nil {
			return err
		}
		types[i] = columnType(def)
	}
	if _, err := s.c.readPacket(); err != nil { // EOF
		return err
	}
	if cap(s.c.cols) < int(nCols) {
		s.c.cols = make([][]byte, nCols)
	}
	s.c.cols = s.c.cols[:nCols]

	for {
		p, err := s.c.readPacket()
		if err != nil {
			return err
		}
		if p[0] == 0xfe && len(p) < 9 {
			return nil
		}
		if p[0] == 0xff {
			return errors.New(string(p[9:]))
		}
		// Binary row: 0x00, then a null bitmap offset by two bits, then the
		// values back to back in their wire encodings.
		nullMap := p[1 : 1+(int(nCols)+9)/8]
		off := 1 + len(nullMap)
		for i := 0; i < int(nCols); i++ {
			if nullMap[(i+2)/8]&(1<<uint((i+2)%8)) != 0 {
				s.c.cols[i] = nil
				continue
			}
			w := fixedWidth(types[i])
			if w > 0 {
				s.c.cols[i] = p[off : off+w]
				off += w
				continue
			}
			// Length-encoded — and here the driver's contract with
			// runtime/mydec is ASYMMETRIC, which the first run of this got
			// wrong.
			//
			// A string or decimal wants the PAYLOAD: mydec.Text and
			// mydec.Decimal read the bytes themselves. A temporal wants the
			// LENGTH PREFIX INCLUDED: mydec.DateTime reads b[0] as the
			// component count and switches on 0/4/7/11, because MySQL packs a
			// datetime component-wise and the length is how you know whether
			// the microseconds are there (ADR-0007).
			//
			// Strip it for one and keep it for the other, or the temporals
			// fail with "wrong length" while every other column looks fine.
			n, adv, _ := lenEncInt(p[off:])
			if isTemporal(types[i]) {
				s.c.cols[i] = p[off : off+adv+int(n)]
			} else {
				s.c.cols[i] = p[off+adv : off+adv+int(n)]
			}
			off += adv + int(n)
		}
		if err := fn(s.c.cols); err != nil {
			return err
		}
	}
}

// isTemporal reports whether mydec wants this column's length prefix kept.
func isTemporal(t byte) bool {
	switch t {
	case typeDateTime, typeDate, typeTime, 0x07: // 0x07 is TIMESTAMP
		return true
	}
	return false
}

// fixedWidth is the wire width of a fixed-size binary type, or 0 for a
// length-encoded one.
func fixedWidth(t byte) int {
	switch t {
	case typeTiny:
		return 1
	case typeShort:
		return 2
	case typeLong:
		return 4
	case typeLongLong, typeDouble:
		return 8
	}
	return 0
}

// columnType pulls the type byte out of a column definition packet. The layout
// is a run of length-encoded strings — catalog, schema, table, org_table, name,
// org_name — then a length-encoded 0x0c, charset, length, and the type.
func columnType(p []byte) byte {
	off := 0
	for i := 0; i < 6; i++ {
		n, w, _ := lenEncInt(p[off:])
		off += w + int(n)
	}
	_, w, _ := lenEncInt(p[off:]) // the 0x0c fixed-length marker
	off += w
	off += 2 // charset
	off += 4 // column length
	return p[off]
}

func appendLenEnc(b, v []byte) []byte {
	switch {
	case len(v) < 251:
		b = append(b, byte(len(v)))
	default:
		b = append(b, 0xfc)
		b = binary.LittleEndian.AppendUint16(b, uint16(len(v)))
	}
	return append(b, v...)
}
