package msdrv

import (
	"encoding/binary"
	"math"
)

// The type system, and the contract this adapter offers runtime/msdec.
//
// RawValues hands back the VALUE BYTES of each column with no length prefix,
// and nil for NULL. For most types that is a sub-slice of the packet buffer and
// costs nothing. Four families cannot be handed over raw, because their wire
// form is not self-describing — the information a decoder needs lives in the
// COLUMN METADATA, which the decoder never sees:
//
//   - uniqueidentifier is MIXED-ENDIAN on the wire. The first three groups are
//     little-endian and the last two big-endian, so the canonical form a uuid
//     is printed and indexed in is a byte swap away. Handing it over raw
//     produces a uuid that round-trips through storm and matches nothing
//     anybody else wrote.
//   - decimal and numeric carry their SCALE in the metadata and a sign byte
//     plus a little-endian magnitude in the value. runtime.Decimal is an
//     unscaled int64 and a scale, and the scale is not recoverable from the
//     bytes.
//   - time, datetime2 and datetimeoffset carry their SCALE in the metadata too,
//     and the value's WIDTH depends on it — 3, 4 or 5 bytes for the time part —
//     so even the length does not say which. Worse, scales 0, 1 and 2 all
//     produce three bytes and mean different things.
//   - datetime and smalldatetime are a different encoding entirely: days since
//     1900 and three-hundredths of a second.
//
// So those are NORMALISED into a canonical form as they are read, in a scratch
// arena reused for the life of the result set. The cost is a copy of at most
// sixteen bytes for the columns that need it and nothing at all for the ones
// that do not, and what msdec receives is a form that means one thing.

// TDS type ids. Named rather than inlined because several differ by one bit
// from another that is decoded completely differently.
const (
	typeNull        = 0x1F
	typeInt1        = 0x30
	typeBit         = 0x32
	typeInt2        = 0x34
	typeInt4        = 0x38
	typeSmallDate   = 0x3A
	typeFloat4      = 0x3B
	typeMoney       = 0x3C
	typeDateTime    = 0x3D
	typeFloat8      = 0x3E
	typeMoney4      = 0x7A
	typeInt8        = 0x7F
	typeGUID        = 0x24
	typeIntN        = 0x26
	typeDecimal     = 0x37
	typeNumeric     = 0x3F
	typeBitN        = 0x68
	typeDecimalN    = 0x6A
	typeNumericN    = 0x6C
	typeFloatN      = 0x6D
	typeMoneyN      = 0x6E
	typeDateTimeN   = 0x6F
	typeDate        = 0x28
	typeTime        = 0x29
	typeDateTime2   = 0x2A
	typeDateTimeOff = 0x2B
	typeChar        = 0x2F
	typeVarChar     = 0x27
	typeBinary      = 0x2D
	typeVarBinary   = 0x25
	typeBigVarBin   = 0xA5
	typeBigVarChar  = 0xA7
	typeBigBinary   = 0xAD
	typeBigChar     = 0xAF
	typeNVarChar    = 0xE7
	typeNChar       = 0xEF
	typeText        = 0x23
	typeImage       = 0x22
	typeNText       = 0x63
	typeXML         = 0xF1
	typeUDT         = 0xF0
)

// plpNull and plpUnknown are the two reserved PLP lengths.
const (
	plpNull    = 0xFFFFFFFFFFFFFFFF
	plpUnknown = 0xFFFFFFFFFFFFFFFE
)

// column is one result column's metadata.
type column struct {
	name string
	id   byte
	// size is the fixed width for a fixed-length type, else 0.
	size int
	// lenBytes is how wide the row's length prefix is: 0, 1, 2 or 4.
	lenBytes int
	// plp says the value arrives in chunks rather than with a single length.
	plp bool
	// scale and prec come from the metadata for the types that carry them.
	scale byte
	prec  byte
	// nullByFF marks the legacy types where 0xFF is NULL and 0 is an empty
	// value, rather than 0 being NULL. Reading those the other way turns every
	// empty string into a NULL.
	nullByFF bool
}

// readColMetadata parses a COLMETADATA token into cols, reusing its backing
// array: a result set's shape is fixed, and the same query run twice should not
// allocate a column list twice.
func (x *conn) readColMetadata(cols []column) ([]column, error) {
	n, err := x.readU16()
	if err != nil {
		return nil, err
	}
	if n == 0xFFFF {
		// "No metadata", which a batch uses to say the shape is unchanged.
		return cols, nil
	}
	if cap(cols) < int(n) {
		cols = make([]column, n)
	}
	cols = cols[:n]
	for i := range cols {
		if _, err := x.readU32(); err != nil { // user type
			return nil, err
		}
		if _, err := x.readU16(); err != nil { // flags
			return nil, err
		}
		c, err := x.readTypeInfo()
		if err != nil {
			return nil, err
		}
		name, err := x.readBVarchar()
		if err != nil {
			return nil, err
		}
		c.name = name
		cols[i] = c
	}
	return cols, nil
}

// readTypeInfo parses one TYPE_INFO.
func (x *conn) readTypeInfo() (column, error) {
	var c column
	id, err := x.readByte()
	if err != nil {
		return c, err
	}
	c.id = id

	switch id {
	// Fixed length: the width is the type, and neither the metadata nor the
	// row carries one.
	case typeNull:
		c.size = 0
	case typeInt1, typeBit:
		c.size = 1
	case typeInt2:
		c.size = 2
	case typeInt4, typeFloat4, typeSmallDate, typeMoney4:
		c.size = 4
	case typeInt8, typeFloat8, typeMoney, typeDateTime:
		c.size = 8

	// One length byte in the metadata, one in the row.
	case typeGUID, typeIntN, typeBitN, typeFloatN, typeMoneyN, typeDateTimeN:
		if _, err := x.readByte(); err != nil {
			return c, err
		}
		c.lenBytes = 1

	case typeDecimal, typeNumeric, typeDecimalN, typeNumericN:
		if _, err := x.readByte(); err != nil { // declared max length
			return c, err
		}
		if c.prec, err = x.readByte(); err != nil {
			return c, err
		}
		if c.scale, err = x.readByte(); err != nil {
			return c, err
		}
		c.lenBytes = 1

	case typeDate:
		// No scale and no metadata length: a date is three bytes or absent.
		c.lenBytes = 1

	case typeTime, typeDateTime2, typeDateTimeOff:
		if c.scale, err = x.readByte(); err != nil {
			return c, err
		}
		c.lenBytes = 1

	// The legacy byte-length strings and binaries, where 0xFF is NULL.
	case typeChar, typeVarChar, typeBinary, typeVarBinary:
		if _, err := x.readByte(); err != nil {
			return c, err
		}
		c.lenBytes = 1
		c.nullByFF = true

	// Two length bytes. 0xFFFF in the METADATA means MAX, which changes the
	// ROW encoding to PLP — a length in the type declaration deciding the
	// framing of the value is unique to this family and easy to miss.
	case typeBigVarBin, typeBigBinary:
		n, err := x.readU16()
		if err != nil {
			return c, err
		}
		c.lenBytes, c.plp = 2, n == 0xFFFF

	case typeBigVarChar, typeBigChar, typeNVarChar, typeNChar:
		n, err := x.readU16()
		if err != nil {
			return c, err
		}
		if err := x.skip(5); err != nil { // collation
			return c, err
		}
		c.lenBytes, c.plp = 2, n == 0xFFFF

	case typeXML:
		// A schema presence byte, and then optional schema names.
		b, err := x.readByte()
		if err != nil {
			return c, err
		}
		if b != 0 {
			for i := 0; i < 2; i++ {
				if _, err := x.readBVarchar(); err != nil {
					return c, err
				}
			}
			if _, err := x.readUSVarchar(); err != nil {
				return c, err
			}
		}
		c.plp = true

	case typeText, typeNText, typeImage:
		if _, err := x.readU32(); err != nil { // max length
			return c, err
		}
		if id != typeImage {
			if err := x.skip(5); err != nil { // collation
				return c, err
			}
		}
		// The table name, as a part-count and that many US_VARCHARs.
		parts, err := x.readByte()
		if err != nil {
			return c, err
		}
		for i := 0; i < int(parts); i++ {
			if _, err := x.readUSVarchar(); err != nil {
				return c, err
			}
		}
		c.lenBytes = 4

	default:
		return c, protoErr("no decoder for TDS type 0x%02X — storm's DDL emits none of "+
			"the types this could be (a UDT, or a deprecated large-object type); a raw "+
			"query that selects one has to CAST it", id)
	}
	return c, nil
}

// arena is the per-result scratch the normalised values are written into.
//
// One buffer, reset at the start of every row, so a value that needs rewriting
// costs a copy and never an allocation. Values handed to the scanner point into
// it and are valid until the next Next(), which is the same lifetime the packet
// buffer's sub-slices already have.
type arena struct {
	buf []byte
	pos int
}

func (a *arena) reset() { a.pos = 0 }

func (a *arena) take(n int) []byte {
	if a.pos+n > len(a.buf) {
		size := len(a.buf)*2 + n
		if size < 256 {
			size = 256
		}
		nb := make([]byte, size)
		copy(nb, a.buf[:a.pos])
		a.buf = nb
	}
	b := a.buf[a.pos : a.pos+n : a.pos+n]
	a.pos += n
	return b
}

// readValue reads one column's value, normalising the families that need it.
//
// scratch is for a value that straddles a packet boundary; ar is for the
// normalised forms. Returns nil for NULL, which is the only thing nil means.
func (x *conn) readValue(c *column, scratch *[]byte, ar *arena) ([]byte, error) {
	raw, err := x.readRaw(c, scratch)
	if err != nil || raw == nil {
		return nil, err
	}
	switch c.id {
	case typeGUID:
		return swapGUID(raw, ar), nil
	case typeDecimal, typeNumeric, typeDecimalN, typeNumericN:
		return normalDecimal(raw, c.scale, ar), nil
	case typeMoney, typeMoneyN, typeMoney4:
		return normalMoney(raw, ar), nil
	case typeTime:
		return normalTime(raw, c.scale, ar), nil
	case typeDateTime2:
		return normalDateTime2(raw, c.scale, ar), nil
	case typeDateTimeOff:
		return normalDateTimeOffset(raw, c.scale, ar), nil
	case typeDateTime, typeSmallDate, typeDateTimeN:
		return normalLegacyDateTime(raw, ar), nil
	}
	return raw, nil
}

// readRaw reads the value's bytes as the wire carries them.
func (x *conn) readRaw(c *column, scratch *[]byte) ([]byte, error) {
	if c.plp {
		return x.readPLP(scratch)
	}
	switch c.lenBytes {
	case 0:
		return x.slice(c.size, scratch)
	case 1:
		n, err := x.readByte()
		if err != nil {
			return nil, err
		}
		if c.nullByFF {
			if n == 0xFF {
				return nil, nil
			}
		} else if n == 0 {
			return nil, nil
		}
		return x.slice(int(n), scratch)
	case 2:
		n, err := x.readU16()
		if err != nil {
			return nil, err
		}
		if n == 0xFFFF {
			return nil, nil
		}
		return x.slice(int(n), scratch)
	case 4:
		// The large-object types put a text pointer in front of the value, and
		// a NULL is a zero-length pointer rather than a length of -1.
		tp, err := x.readByte()
		if err != nil {
			return nil, err
		}
		if tp == 0 {
			return nil, nil
		}
		if err := x.skip(int(tp) + 8); err != nil { // pointer and timestamp
			return nil, err
		}
		n, err := x.readU32()
		if err != nil {
			return nil, err
		}
		if n == 0xFFFFFFFF {
			return nil, nil
		}
		return x.slice(int(n), scratch)
	}
	return nil, protoErr("column %q has no length encoding", c.name)
}

// readPLP reads a partially length-prefixed value: a total, then chunks.
//
// The total may be plpUnknown, which is how the server streams a value whose
// length it does not know when it starts — so the chunks have to be read until
// the terminator either way, and the total is only a hint for sizing.
func (x *conn) readPLP(scratch *[]byte) ([]byte, error) {
	total, err := x.readU64()
	if err != nil {
		return nil, err
	}
	if total == plpNull {
		return nil, nil
	}
	size := 0
	if total != plpUnknown {
		size = int(total)
	}
	if cap(*scratch) < size {
		*scratch = make([]byte, size)
	}
	out := (*scratch)[:0]
	for {
		n, err := x.readU32()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		start := len(out)
		if start+int(n) > cap(out) {
			nb := make([]byte, start+int(n))
			copy(nb, out)
			*scratch = nb
			out = nb[:start]
		}
		out = out[:start+int(n)]
		if err := x.readFull(out[start:]); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		// An empty value is not a NULL, and returning nil would make it one.
		return (*scratch)[:0], nil
	}
	return out, nil
}

// swapGUID puts a uniqueidentifier into canonical byte order.
//
// The wire form is the Windows GUID layout: Data1 (4 bytes) and Data2 and Data3
// (2 each) are LITTLE-endian, and the remaining eight bytes are in order. The
// canonical form every other system prints and indexes is big-endian
// throughout. Getting this wrong is invisible in storm's own round trip — write
// it scrambled, read it scrambled, and it matches — and visible the moment
// anything else reads the column.
func swapGUID(b []byte, ar *arena) []byte {
	if len(b) != 16 {
		return b
	}
	out := ar.take(16)
	out[0], out[1], out[2], out[3] = b[3], b[2], b[1], b[0]
	out[4], out[5] = b[5], b[4]
	out[6], out[7] = b[7], b[6]
	copy(out[8:], b[8:])
	return out
}

// decimalOverflow is the scale byte that says the value needs more than the 18
// significant digits runtime.Decimal holds. msdec turns it into
// runtime.ErrDecimalRange rather than a silently truncated number.
const decimalOverflow = 0xFF

// normalDecimal rewrites a decimal into [scale][int64 little-endian].
//
// The wire form is a sign byte then a little-endian magnitude of 4, 8, 12 or 16
// bytes, with the SCALE in the column metadata — which the decoder never sees.
// Nine bytes of canonical form is what lets msdec.Decimal be a function of its
// argument.
func normalDecimal(b []byte, scale byte, ar *arena) []byte {
	out := ar.take(9)
	if len(b) < 2 {
		out[0] = decimalOverflow
		return out
	}
	neg := b[0] == 0
	mag := b[1:]
	var v uint64
	for i := len(mag) - 1; i >= 0; i-- {
		if mag[i] == 0 {
			continue
		}
		if i >= 8 {
			// Beyond 64 bits. Flagged rather than truncated: a truncated
			// currency figure that looks plausible is the worst outcome
			// available here.
			out[0] = decimalOverflow
			return out
		}
		break
	}
	for i := 0; i < len(mag) && i < 8; i++ {
		v |= uint64(mag[i]) << (8 * i)
	}
	if v > math.MaxInt64 {
		out[0] = decimalOverflow
		return out
	}
	n := int64(v)
	if neg {
		n = -n
	}
	out[0] = scale
	binary.LittleEndian.PutUint64(out[1:], uint64(n))
	return out
}

// normalMoney rewrites money and smallmoney into the same nine bytes.
//
// Both are integers of ten-thousandths, so the scale is always 4 — but money is
// EIGHT bytes with the HIGH word first, which is the one thing about this type
// that surprises everyone who reads it as a plain int64.
func normalMoney(b []byte, ar *arena) []byte {
	out := ar.take(9)
	out[0] = 4
	switch len(b) {
	case 4:
		binary.LittleEndian.PutUint64(out[1:], uint64(int64(int32(binary.LittleEndian.Uint32(b)))))
	case 8:
		hi := int64(int32(binary.LittleEndian.Uint32(b[0:4])))
		lo := int64(binary.LittleEndian.Uint32(b[4:8]))
		binary.LittleEndian.PutUint64(out[1:], uint64(hi<<32|lo))
	default:
		out[0] = decimalOverflow
	}
	return out
}

// timeUnits reads a time-of-day of the given scale and rescales it to 100 ns.
func timeUnits(b []byte, scale byte) uint64 {
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	for s := scale; s < 7; s++ {
		v *= 10
	}
	return v
}

// timeWidth is how many bytes a time of this scale occupies.
func timeWidth(scale byte) int {
	switch {
	case scale <= 2:
		return 3
	case scale <= 4:
		return 4
	default:
		return 5
	}
}

// normalTime rewrites a time to five bytes of 100 ns units — the scale-7 form,
// whatever the column's declared scale.
func normalTime(b []byte, scale byte, ar *arena) []byte {
	out := ar.take(5)
	v := timeUnits(b, scale)
	for i := 0; i < 5; i++ {
		out[i] = byte(v >> (8 * i))
	}
	return out
}

// normalDateTime2 rewrites to five bytes of time then three of date, which is
// the scale-7 layout.
func normalDateTime2(b []byte, scale byte, ar *arena) []byte {
	w := timeWidth(scale)
	out := ar.take(8)
	if len(b) < w+3 {
		return out
	}
	v := timeUnits(b[:w], scale)
	for i := 0; i < 5; i++ {
		out[i] = byte(v >> (8 * i))
	}
	copy(out[5:], b[w:w+3])
	return out
}

// normalDateTimeOffset rewrites to the scale-7 datetime2 layout followed by the
// two-byte signed offset in minutes.
func normalDateTimeOffset(b []byte, scale byte, ar *arena) []byte {
	w := timeWidth(scale)
	out := ar.take(10)
	if len(b) < w+5 {
		return out
	}
	v := timeUnits(b[:w], scale)
	for i := 0; i < 5; i++ {
		out[i] = byte(v >> (8 * i))
	}
	copy(out[5:], b[w:w+3])
	copy(out[8:], b[w+3:w+5])
	return out
}

// Days between 1900-01-01, which datetime counts from, and 0001-01-01, which
// date and datetime2 count from.
const daysTo1900 = 693595

// normalLegacyDateTime rewrites datetime and smalldatetime into the datetime2
// layout, so msdec has one temporal decoder rather than three.
//
// datetime is days since 1900 and THREE-HUNDREDTHS of a second — a unit chosen
// in the 1980s that cannot represent a third of the milliseconds in a second,
// which is why its resolution is quoted as 3.33 ms. smalldatetime is days and
// whole minutes.
func normalLegacyDateTime(b []byte, ar *arena) []byte {
	out := ar.take(8)
	var days int32
	var units uint64
	switch len(b) {
	case 8:
		days = int32(binary.LittleEndian.Uint32(b[0:4]))
		ticks := uint64(binary.LittleEndian.Uint32(b[4:8]))
		// 300 ticks per second, 10,000,000 hundred-nanoseconds per second.
		units = ticks * 10000000 / 300
	case 4:
		days = int32(binary.LittleEndian.Uint16(b[0:2]))
		units = uint64(binary.LittleEndian.Uint16(b[2:4])) * 60 * 10000000
	default:
		return out
	}
	for i := 0; i < 5; i++ {
		out[i] = byte(units >> (8 * i))
	}
	d := uint32(days + daysTo1900)
	out[5], out[6], out[7] = byte(d), byte(d>>8), byte(d>>16)
	return out
}
