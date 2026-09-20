package msdrv

import (
	"context"
	"encoding/binary"
	"math"
	"strconv"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// Sending a statement, and the one decision that shapes the whole file:
// storm's parameters are NAMED, so a statement goes through sp_executesql
// rather than being spliced.
//
// sp_executesql takes the statement, a DECLARATION of its parameters —
// "@p1 bigint,@p2 nvarchar(4000)" — and then the values, named. The server
// caches the plan against the text and the declaration, which is exactly the
// bargain storm's shape cache already makes on its own side: one statement text
// per query SHAPE, so a plan is compiled once and reused for the life of the
// process. Splicing values into the text instead would mint a plan per request,
// which is the server-side version of the N+1 this library exists to prevent.
//
// A statement with NO parameters skips the wrapper and goes as a plain SQL
// batch. There is nothing to declare, and the round trip is a packet smaller.

// sp_executesql's well-known procedure id. Sending the id rather than the name
// saves the server a name lookup and the wire twenty-six bytes.
const procExecuteSQL = 10

// writeHeaders writes the transaction-descriptor header every TDS 7.2+ request
// carries.
//
// The descriptor is the CURRENT transaction's, learned from the ENVCHANGE the
// server sends on BEGIN. Sending zero works until the first BEGIN TRANSACTION
// and then fails every statement inside it — the server answers "New request is
// not allowed to start because it should come with valid transaction
// descriptor", which names the field and not the mistake.
func (x *conn) writeHeaders() error {
	var b [22]byte
	binary.LittleEndian.PutUint32(b[0:], 22)     // total length
	binary.LittleEndian.PutUint32(b[4:], 18)     // this header's length
	binary.LittleEndian.PutUint16(b[8:], 2)      // type: transaction descriptor
	binary.LittleEndian.PutUint64(b[10:], x.txn) // descriptor
	binary.LittleEndian.PutUint32(b[18:], 1)     // outstanding requests
	return x.write(b[:])
}

// send writes one statement and its arguments.
func (c *Conn) send(ctx context.Context, sql string, args []any) error {
	if c.bad {
		return protoErr("this connection's token stream is out of step and it cannot be reused")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) == 0 {
		return c.sendBatch(sql)
	}
	return c.sendRPC(sql, args)
}

// sendBatch sends a statement with no parameters.
func (c *Conn) sendBatch(sql string) error {
	x := c.x
	x.begin(pktSQLBatch)
	if err := x.writeHeaders(); err != nil {
		return err
	}
	if err := x.writeUCS2(sql); err != nil {
		return err
	}
	return x.end()
}

// sendRPC sends a statement through sp_executesql.
func (c *Conn) sendRPC(sql string, args []any) error {
	x := c.x
	decl, err := declare(args)
	if err != nil {
		return err
	}

	x.begin(pktRPC)
	if err := x.writeHeaders(); err != nil {
		return err
	}
	// 0xFFFF introduces a procedure ID rather than a name.
	if err := x.writeU16(0xFFFF); err != nil {
		return err
	}
	if err := x.writeU16(procExecuteSQL); err != nil {
		return err
	}
	if err := x.writeU16(0); err != nil { // option flags
		return err
	}

	// The two unnamed parameters sp_executesql itself takes.
	if err := x.writeParamNVarCharMax("", sql); err != nil {
		return err
	}
	if err := x.writeParamNVarCharMax("", decl); err != nil {
		return err
	}
	for i, a := range args {
		if err := x.writeParam("@p"+strconv.Itoa(i+1), a); err != nil {
			return err
		}
	}
	return x.end()
}

// declare builds the parameter declaration sp_executesql binds against.
//
// The types here must agree with the TYPE_INFO written for each value below. A
// disagreement is not a conversion — the server binds by NAME and types by this
// string, so a value sent as a bigint against a declaration of nvarchar is a
// conversion error naming a parameter the caller never wrote.
func declare(args []any) (string, error) {
	var b []byte
	for i, a := range args {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '@', 'p')
		b = strconv.AppendInt(b, int64(i+1), 10)
		b = append(b, ' ')
		t, err := declType(a)
		if err != nil {
			return "", err
		}
		b = append(b, t...)
	}
	return string(b), nil
}

// declType is a value's SQL type as the declaration spells it.
func declType(a any) (string, error) {
	if _, ok := jsonList(a); ok {
		// A bound key list. One nvarchar(max) document, unpacked server-side by
		// OPENJSON — see compile/mssql, and ADR-0010 for why the alternative
		// makes the statement's shape a function of request data.
		return "nvarchar(max)", nil
	}
	switch v := a.(type) {
	case nil:
		// A typed NULL has to be SOMETHING. nvarchar takes an implicit
		// conversion to anything storm declares, which is the least wrong
		// choice available without the column's type.
		return "nvarchar(4000)", nil
	case bool, *bool:
		return "bit", nil
	case int8, *int8:
		return "tinyint", nil
	case int16, *int16:
		return "smallint", nil
	case int32, *int32:
		return "int", nil
	case int, int64, uint64, *int, *int64:
		return "bigint", nil
	case float32, *float32:
		return "real", nil
	case float64, *float64:
		return "float", nil
	case string:
		if len(v) > 4000 {
			return "nvarchar(max)", nil
		}
		return "nvarchar(4000)", nil
	case *string:
		if v != nil && len(*v) > 4000 {
			return "nvarchar(max)", nil
		}
		return "nvarchar(4000)", nil
	case []byte, *[]byte:
		return "varbinary(max)", nil
	case [16]byte:
		return "uniqueidentifier", nil
	case time.Time, *time.Time:
		return "datetimeoffset(7)", nil
	case runtime.TimeOfDay:
		return "time(7)", nil
	case runtime.Decimal:
		return "decimal(38," + strconv.Itoa(int(v.Scale)) + ")", nil
	}
	if _, ok := a.(interface{ String() string }); ok {
		return "nvarchar(4000)", nil
	}
	return "", protoErr("no binding for %T", a)
}

// ---- parameter encoding -----------------------------------------------------

// writeParamHeader writes a parameter's name and status.
func (x *conn) writeParamHeader(name string) error {
	n := 0
	for range name {
		n++
	}
	if err := x.writeByte(byte(n)); err != nil {
		return err
	}
	if err := x.writeUCS2(name); err != nil {
		return err
	}
	return x.writeByte(0) // status: input
}

// writeParamNVarCharMax writes an nvarchar(max) parameter, which is how the
// statement and the declaration themselves travel.
func (x *conn) writeParamNVarCharMax(name, s string) error {
	if err := x.writeParamHeader(name); err != nil {
		return err
	}
	if err := x.writeByte(typeNVarChar); err != nil {
		return err
	}
	if err := x.writeU16(0xFFFF); err != nil { // MAX
		return err
	}
	if err := x.write([]byte{0, 0, 0, 0, 0}); err != nil { // collation
		return err
	}
	return x.writePLP(s)
}

// writePLP writes a value in the partially-length-prefixed form: a total, one
// chunk, and the terminator.
//
// One chunk rather than several because the length IS known here — storm never
// streams a parameter it has not already built — and the server accepts any
// chunking. The terminator is not optional: without it the server waits for
// more of a value that is already complete.
func (x *conn) writePLP(s string) error {
	n := ucs2Len(s)
	if err := x.writeU64(uint64(n)); err != nil {
		return err
	}
	if n > 0 {
		if err := x.writeU32(uint32(n)); err != nil {
			return err
		}
		if err := x.writeUCS2(s); err != nil {
			return err
		}
	}
	return x.writeU32(0)
}

// writePLPBytes is writePLP for a byte slice.
func (x *conn) writePLPBytes(b []byte) error {
	if err := x.writeU64(uint64(len(b))); err != nil {
		return err
	}
	if len(b) > 0 {
		if err := x.writeU32(uint32(len(b))); err != nil {
			return err
		}
		if err := x.write(b); err != nil {
			return err
		}
	}
	return x.writeU32(0)
}

// writeParam writes one named argument.
func (x *conn) writeParam(name string, a any) error {
	if doc, ok := jsonList(a); ok {
		return x.writeParamNVarCharMax(name, doc)
	}
	if err := x.writeParamHeader(name); err != nil {
		return err
	}
	return x.writeValue(a)
}

// writeValue writes a parameter's TYPE_INFO and its value.
//
// The pointer cases are not a convenience: storm's binder passes pointers into
// its own reusable buffers — &b.limit, &b.offset — so a nil pointer is the way
// a NULL arrives, and dereferencing one without checking is the defect that hung
// a bulk insert on MySQL (a typed nil inside a Null[T]).
func (x *conn) writeValue(a any) error {
	switch v := a.(type) {
	case nil:
		return x.nullNVarChar()

	case bool:
		return x.fixedN(typeBitN, 1, []byte{b2i(v)})
	case *bool:
		if v == nil {
			return x.fixedN(typeBitN, 1, nil)
		}
		return x.fixedN(typeBitN, 1, []byte{b2i(*v)})

	case int8:
		return x.intN(1, int64(v))
	case *int8:
		if v == nil {
			return x.fixedN(typeIntN, 1, nil)
		}
		return x.intN(1, int64(*v))
	case int16:
		return x.intN(2, int64(v))
	case *int16:
		if v == nil {
			return x.fixedN(typeIntN, 2, nil)
		}
		return x.intN(2, int64(*v))
	case int32:
		return x.intN(4, int64(v))
	case *int32:
		if v == nil {
			return x.fixedN(typeIntN, 4, nil)
		}
		return x.intN(4, int64(*v))
	case int:
		return x.intN(8, int64(v))
	case *int:
		if v == nil {
			return x.fixedN(typeIntN, 8, nil)
		}
		return x.intN(8, int64(*v))
	case int64:
		return x.intN(8, v)
	case *int64:
		if v == nil {
			return x.fixedN(typeIntN, 8, nil)
		}
		return x.intN(8, *v)
	case uint64:
		return x.intN(8, int64(v))

	case float32:
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
		return x.fixedN(typeFloatN, 4, b[:])
	case float64:
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
		return x.fixedN(typeFloatN, 8, b[:])
	case *float64:
		if v == nil {
			return x.fixedN(typeFloatN, 8, nil)
		}
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(*v))
		return x.fixedN(typeFloatN, 8, b[:])

	case string:
		return x.nvarchar(v)
	case *string:
		if v == nil {
			return x.nullNVarChar()
		}
		return x.nvarchar(*v)

	case []byte:
		if v == nil {
			return x.nullVarBinary()
		}
		return x.varbinary(v)
	case *[]byte:
		if v == nil || *v == nil {
			return x.nullVarBinary()
		}
		return x.varbinary(*v)

	case [16]byte:
		return x.guid(v)

	case time.Time:
		return x.datetimeoffset(v)
	case *time.Time:
		if v == nil {
			if err := x.writeByte(typeDateTimeOff); err != nil {
				return err
			}
			if err := x.writeByte(7); err != nil {
				return err
			}
			return x.writeByte(0)
		}
		return x.datetimeoffset(*v)

	case runtime.TimeOfDay:
		return x.timeOfDay(v)

	case runtime.Decimal:
		return x.decimal(v)
	}
	if sr, ok := a.(interface{ String() string }); ok {
		return x.nvarchar(sr.String())
	}
	return protoErr("no binding for %T", a)
}

func b2i(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// fixedN writes one of the nullable fixed-width types: the id, the declared
// width, then the value's own length and bytes. A nil value is a length of
// zero, which is how every N-type spells NULL.
func (x *conn) fixedN(id byte, width int, val []byte) error {
	if err := x.writeByte(id); err != nil {
		return err
	}
	if err := x.writeByte(byte(width)); err != nil {
		return err
	}
	if val == nil {
		return x.writeByte(0)
	}
	if err := x.writeByte(byte(len(val))); err != nil {
		return err
	}
	return x.write(val)
}

func (x *conn) intN(width int, v int64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(v))
	return x.fixedN(typeIntN, width, b[:width])
}

func (x *conn) nvarchar(s string) error {
	n := ucs2Len(s)
	if n > 8000 {
		// Past 4000 characters nvarchar needs the MAX form, which is PLP.
		if err := x.writeByte(typeNVarChar); err != nil {
			return err
		}
		if err := x.writeU16(0xFFFF); err != nil {
			return err
		}
		if err := x.write([]byte{0, 0, 0, 0, 0}); err != nil {
			return err
		}
		return x.writePLP(s)
	}
	if err := x.writeByte(typeNVarChar); err != nil {
		return err
	}
	// The DECLARED width, which must cover the value. 8000 bytes is
	// nvarchar(4000), the widest non-MAX form.
	if err := x.writeU16(8000); err != nil {
		return err
	}
	if err := x.write([]byte{0, 0, 0, 0, 0}); err != nil {
		return err
	}
	if err := x.writeU16(uint16(n)); err != nil {
		return err
	}
	return x.writeUCS2(s)
}

func (x *conn) nullNVarChar() error {
	if err := x.writeByte(typeNVarChar); err != nil {
		return err
	}
	if err := x.writeU16(8000); err != nil {
		return err
	}
	if err := x.write([]byte{0, 0, 0, 0, 0}); err != nil {
		return err
	}
	return x.writeU16(0xFFFF)
}

func (x *conn) varbinary(b []byte) error {
	if err := x.writeByte(typeBigVarBin); err != nil {
		return err
	}
	if err := x.writeU16(0xFFFF); err != nil { // MAX
		return err
	}
	return x.writePLPBytes(b)
}

func (x *conn) nullVarBinary() error {
	if err := x.writeByte(typeBigVarBin); err != nil {
		return err
	}
	if err := x.writeU16(0xFFFF); err != nil {
		return err
	}
	return x.writeU64(plpNull)
}

// guid writes a uuid, swapping it back into the wire's mixed-endian layout.
//
// The mirror of swapGUID in types.go, and it has to exist for the same reason:
// storm holds the canonical order, the wire wants the Windows one, and a client
// that skips both swaps is self-consistent and wrong against everything else.
func (x *conn) guid(u [16]byte) error {
	var b [16]byte
	b[0], b[1], b[2], b[3] = u[3], u[2], u[1], u[0]
	b[4], b[5] = u[5], u[4]
	b[6], b[7] = u[7], u[6]
	copy(b[8:], u[8:])
	return x.fixedN(typeGUID, 16, b[:])
}

// Days from 0001-01-01 to the Unix epoch, which is what date counts from.
const daysToEpoch = 719162

// datetimeoffset writes a time as the scale-7 form: 100 ns since midnight UTC,
// the day number, and the offset in minutes.
func (x *conn) datetimeoffset(t time.Time) error {
	if err := x.writeByte(typeDateTimeOff); err != nil {
		return err
	}
	if err := x.writeByte(7); err != nil { // scale
		return err
	}
	if err := x.writeByte(10); err != nil { // length
		return err
	}
	_, off := t.Zone()
	utc := t.UTC()
	units := uint64(utc.Hour())*3600 + uint64(utc.Minute())*60 + uint64(utc.Second())
	units = units*10000000 + uint64(utc.Nanosecond())/100
	var b [10]byte
	for i := 0; i < 5; i++ {
		b[i] = byte(units >> (8 * i))
	}
	days := uint32(utc.Unix()/86400) + daysToEpoch
	if utc.Unix() < 0 {
		days = uint32((utc.Unix()-86399)/86400) + daysToEpoch
	}
	b[5], b[6], b[7] = byte(days), byte(days>>8), byte(days>>16)
	m := int16(off / 60)
	b[8], b[9] = byte(uint16(m)), byte(uint16(m)>>8)
	return x.write(b[:])
}

// timeOfDay writes a runtime.TimeOfDay, which counts MICROSECONDS where this
// type counts hundreds of nanoseconds. The factor of ten is the conversion M9
// got wrong in the other direction and paid for with a value a thousand times
// too large.
func (x *conn) timeOfDay(v runtime.TimeOfDay) error {
	if err := x.writeByte(typeTime); err != nil {
		return err
	}
	if err := x.writeByte(7); err != nil {
		return err
	}
	if err := x.writeByte(5); err != nil {
		return err
	}
	units := uint64(v.Duration() / 100)
	var b [5]byte
	for i := 0; i < 5; i++ {
		b[i] = byte(units >> (8 * i))
	}
	return x.write(b[:])
}

// decimal writes a runtime.Decimal as sign plus a little-endian magnitude.
func (x *conn) decimal(d runtime.Decimal) error {
	if err := x.writeByte(typeDecimalN); err != nil {
		return err
	}
	if err := x.writeByte(17); err != nil { // declared max length
		return err
	}
	if err := x.writeByte(38); err != nil { // precision
		return err
	}
	scale := d.Scale
	if scale < 0 || scale > 38 {
		scale = 0
	}
	if err := x.writeByte(byte(scale)); err != nil {
		return err
	}
	u := d.Unscaled
	sign := byte(1)
	if u < 0 {
		sign = 0
		u = -u
	}
	var b [17]byte
	b[0] = sign
	binary.LittleEndian.PutUint64(b[1:], uint64(u))
	if err := x.writeByte(17); err != nil { // value length
		return err
	}
	return x.write(b[:])
}
