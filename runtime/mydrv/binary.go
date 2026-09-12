package mydrv

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"
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
		return nil, parseError(p)
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
		// NULL bitmap first: a bound nil is not a value with a type, it is an
		// absence recorded in the map, and the value section skips it.
		nullMap := make([]byte, (len(args)+7)/8)
		for i, a := range args {
			if isNil(a) {
				nullMap[i/8] |= 1 << uint(i%8)
			}
		}
		b = append(b, nullMap...)
		b = append(b, 1) // new params bound
		for _, a := range args {
			t, unsigned := bindType(deref(a))
			b = append(b, t, unsigned)
		}
		for _, a := range args {
			if isNil(a) {
				continue
			}
			var err error
			if b, err = appendBind(b, deref(a)); err != nil {
				return err
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
		return parseError(p)
	}
	if p[0] == 0x00 {
		if s.c.onOK != nil {
			s.c.onOK(p)
		}
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
			return parseError(p)
		}
		// Binary row: 0x00, then a null bitmap offset by two bits, then the
		// values back to back in their wire encodings.
		if 1+(int(nCols)+9)/8 > len(p) {
			return errShortRow
		}
		nullMap := p[1 : 1+(int(nCols)+9)/8]
		off := 1 + len(nullMap)
		for i := 0; i < int(nCols); i++ {
			if nullMap[(i+2)/8]&(1<<uint((i+2)%8)) != 0 {
				s.c.cols[i] = nil
				continue
			}
			w := fixedWidth(types[i])
			if w > 0 {
				if off+w > len(p) {
					return errShortRow
				}
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
			n, adv, ok := lenEncInt(p[off:])
			// Bounds-checked rather than trusted. A row packet that does not
			// add up is a corrupt or hostile server, and a driver that panics
			// on one hands it the process.
			if !ok || off+adv+int(n) > len(p) {
				return errShortRow
			}
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
// fixedWidth is the binary protocol's width for a type, or 0 for the
// length-encoded ones.
//
// Every fixed-width type MySQL can send has to be here, not just the ones storm
// emits: a type missing from this table is read as length-encoded, so its first
// byte becomes a length and the rest of the ROW is decoded from the wrong
// offset. That is how a FLOAT column made this panic rather than return a wrong
// number — the failure is not confined to the column that caused it.
func fixedWidth(t byte) int {
	switch t {
	case typeTiny:
		return 1
	case typeShort, typeYear:
		return 2
	case typeLong, typeInt24, typeFloat:
		// MEDIUMINT is three bytes in storage and FOUR on the wire.
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

// execAffected runs a statement that returns no rows and reports the affected
// count out of the OK packet.
func (s *stmt) execAffected(args []any, out *int64) error {
	return s.execRaw(args, func(cols [][]byte) error { return nil }, out)
}

// execRaw is exec with the OK packet's affected-rows count captured.
func (s *stmt) execRaw(args []any, fn func(cols [][]byte) error, affected *int64) error {
	s.c.onOK = func(p []byte) {
		if affected == nil {
			return
		}
		n, _, _ := lenEncInt(p[1:])
		*affected = int64(n)
	}
	defer func() { s.c.onOK = nil }()
	return s.exec(args, fn)
}

// Parameter binding.
//
// The type table storm actually needs, which is narrower than MySQL's: the
// generator only ever passes what a column can hold, and a nullable column
// passes nil through Null[T].Arg().

func isNil(a any) bool {
	if a == nil {
		return true
	}
	// A typed nil pointer is a NULL, not a value. Checked without reflect,
	// because scripts/check/boundaries.sh forbids it under runtime/ — one
	// reflection path becomes THE path and the budgets become fiction.
	switch v := a.(type) {
	case *string:
		return v == nil
	case *int64:
		return v == nil
	case *int32:
		return v == nil
	case *int16:
		return v == nil
	case *bool:
		return v == nil
	case *float64:
		return v == nil
	case *time.Time:
		return v == nil
	case *[]byte:
		return v == nil
	}
	return false
}

// deref unwraps a pointer argument to the value it points at.
//
// storm's predicate arena hands back pointers for bound values, so every type
// arrives in both shapes. Written out rather than reflected over, for the
// reason isNil is.
func deref(a any) any {
	switch v := a.(type) {
	case *string:
		return *v
	case *int64:
		return *v
	case *int32:
		return *v
	case *int16:
		return *v
	case *int8:
		return *v
	case *int:
		return *v
	case *uint64:
		return *v
	case *bool:
		return *v
	case *float32:
		return *v
	case *float64:
		return *v
	case *time.Time:
		return *v
	case *time.Duration:
		return *v
	case *[]byte:
		return *v
	case *[16]byte:
		return *v
	}
	return a
}

// bindType is the (type, unsigned) pair the EXECUTE header carries per
// parameter.
func bindType(a any) (byte, byte) {
	switch a.(type) {
	case nil:
		return typeNull, 0
	case bool:
		return typeTiny, 0
	case int8:
		return typeTiny, 0
	case int16:
		return typeShort, 0
	case int32:
		return typeLong, 0
	case int, int64:
		return typeLongLong, 0
	case uint64:
		return typeLongLong, 0x80
	case float32:
		return typeFloat, 0
	case float64:
		return typeDouble, 0
	}
	// Everything else goes as a length-encoded string: uuids, byte slices,
	// decimals and timestamps included. MySQL parses a DATETIME literal into
	// the column's type, and a BINARY(16) takes the bytes as they are — so the
	// wire stays simple and the SERVER does the narrowing it would do anyway.
	return typeVarString, 0
}

const (
	typeNull  = 0x06
	typeFloat = 0x04
	typeInt24 = 0x09
	typeYear  = 0x0d
)

func appendBind(b []byte, a any) ([]byte, error) {
	switch v := a.(type) {
	case bool:
		if v {
			return append(b, 1), nil
		}
		return append(b, 0), nil
	case int8:
		return append(b, byte(v)), nil
	case int16:
		return binary.LittleEndian.AppendUint16(b, uint16(v)), nil
	case int32:
		return binary.LittleEndian.AppendUint32(b, uint32(v)), nil
	case int:
		return binary.LittleEndian.AppendUint64(b, uint64(v)), nil
	case int64:
		return binary.LittleEndian.AppendUint64(b, uint64(v)), nil
	case uint64:
		return binary.LittleEndian.AppendUint64(b, v), nil
	case float32:
		return binary.LittleEndian.AppendUint32(b, math.Float32bits(v)), nil
	case float64:
		return binary.LittleEndian.AppendUint64(b, math.Float64bits(v)), nil
	case string:
		return appendLenEnc(b, []byte(v)), nil
	case []byte:
		return appendLenEnc(b, v), nil
	case [16]byte:
		// A uuid, into BINARY(16). Sent as the sixteen bytes rather than a
		// hyphenated string, because that is what the column holds and
		// converting would make the index unusable.
		return appendLenEnc(b, v[:]), nil
	case time.Time:
		// The literal MySQL parses back into DATETIME(6). Microseconds always,
		// so a value that has them does not lose them silently.
		return appendLenEnc(b, []byte(v.Format("2006-01-02 15:04:05.000000"))), nil
	case time.Duration:
		return appendLenEnc(b, appendDuration(nil, v)), nil
	}
	// Anything with a String() — runtime.Decimal is the one that matters — goes
	// as its text, which is exactly how MySQL wants a DECIMAL bound.
	if sr, ok := a.(interface{ String() string }); ok {
		return appendLenEnc(b, []byte(sr.String())), nil
	}
	return nil, fmt.Errorf("mydrv: no binding for %T", a)
}

// appendDuration renders MySQL's TIME, which is a signed span and not a clock
// reading — it can exceed 24 hours and can be negative.
//
// Written out by hand rather than through a format call, because this is the
// bind path and scripts/check/boundaries.sh refuses formatting there. The rule is about
// the hot path rather than injection — the value is bound, not spliced — but
// binding is per statement and the rule is right that it should not allocate a
// format walk to print eight digits.
func appendDuration(b []byte, d time.Duration) []byte {
	if d < 0 {
		b = append(b, '-')
		d = -d
	}
	h := int64(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int64(d / time.Minute)
	d -= time.Duration(m) * time.Minute
	sec := int64(d / time.Second)
	us := int64((d - time.Duration(sec)*time.Second) / time.Microsecond)
	b = appendPad(b, h, 2)
	b = append(b, ':')
	b = appendPad(b, m, 2)
	b = append(b, ':')
	b = appendPad(b, sec, 2)
	b = append(b, '.')
	return appendPad(b, us, 6)
}

// appendPad writes v zero-padded to at least width digits.
func appendPad(b []byte, v int64, width int) []byte {
	var tmp [20]byte
	i := len(tmp)
	for {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	for n := len(tmp) - i; n < width; n++ {
		b = append(b, '0')
	}
	return append(b, tmp[i:]...)
}
