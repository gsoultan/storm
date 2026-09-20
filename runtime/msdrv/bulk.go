package msdrv

import (
	"context"
	"encoding/binary"
	"math"
	"strconv"
	"time"

	"github.com/gsoultan/storm/runtime"
)

// Bulk load: TDS's own wire path for getting rows in, which is what `bcp` and
// SqlBulkCopy use.
//
// This is a DIFFERENT PACKET TYPE, not a faster loop. A multi-row INSERT still
// parses a statement, builds a plan, fires triggers per statement and writes
// every row through the normal insert path; a bulk load streams rows straight
// at the storage engine. That is why "1,000 inserts = one COPY" is a claim
// storm can make rather than a hope — and why runtime/mydrv, whose protocol has
// no such path, documents an emulation instead.
//
// The exchange has three steps and the middle one is easy to get wrong:
//
//  1. `INSERT BULK [t] ([a] int, [b] nvarchar(255))` as an ordinary SQL batch.
//     It is not a statement that inserts anything — it puts the CONNECTION into
//     bulk mode and declares the shape that is coming.
//  2. A packet of type 7 carrying COLMETADATA, then one ROW token per row, then
//     a DONE. No statement, no parameters: the rows are the message.
//  3. The server's reply, which carries the count.
//
// The column types in step 1 and step 2 must AGREE, and both must agree with
// the table. So they are not guessed from the Go values: the shape is read from
// the server once and cached, and the same column list drives the declaration,
// the metadata and the encoder. Guessing produces a load that either refuses
// every row or silently truncates them.

// bulkKey identifies a cached column shape.
type bulkKey struct {
	table string
	cols  string
}

// bulkMeta reads the target's column types, once per (table, column list).
//
// `SELECT TOP 0` is the probe: it returns no rows and a full COLMETADATA, which
// is exactly the shape the bulk stream has to echo. Cheaper and more honest
// than reading sys.columns, because it is the SAME token the server would send
// for a real read — so a type this client cannot decode is refused here rather
// than halfway through a load.
func bulkKeyFor(table string, cols []string) bulkKey {
	k := bulkKey{table: table}
	for i, col := range cols {
		if i > 0 {
			k.cols += ","
		}
		k.cols += col
	}
	return k
}

func (c *Conn) bulkMeta(ctx context.Context, table string, cols []string) ([]column, error) {
	key := bulkKeyFor(table, cols)
	if m, ok := c.bulk[key]; ok {
		return m, nil
	}

	var q []byte
	q = append(q, "SELECT TOP 0 "...)
	for i, col := range cols {
		if i > 0 {
			q = append(q, ", "...)
		}
		q = append(q, identBracket(col)...)
	}
	q = append(q, " FROM "...)
	q = append(q, identBracket(table)...)

	rows, err := c.Query(ctx, string(q), nil)
	if err != nil {
		return nil, err
	}
	r := rows.(*Rows)
	meta := make([]column, len(r.cols))
	copy(meta, r.cols)
	r.Close()
	if err := r.Err(); err != nil {
		return nil, err
	}
	if len(meta) != len(cols) {
		return nil, protoErr("the server described %d columns for a load of %d",
			len(meta), len(cols))
	}
	if c.bulk == nil {
		c.bulk = map[bulkKey][]column{}
	}
	c.bulk[key] = meta
	return meta, nil
}

// CopyFrom bulk-loads rows through the TDS bulk path.
//
// ONE round trip for the whole load, whatever the row count: the rows stream
// across as many packets as they need, and the server answers once. That is the
// guarantee runtime.Executor documents, and here it is the protocol's own
// rather than an emulation of it.
func (c *Conn) CopyFrom(ctx context.Context, table string, cols []string,
	src runtime.CopySource) (int64, error) {
	if len(cols) == 0 {
		return 0, protoErr("CopyFrom needs at least one column")
	}
	if c.busy {
		return 0, ErrRowsOpen
	}
	key := bulkKeyFor(table, cols)
	meta, err := c.bulkMeta(ctx, table, cols)
	if err != nil {
		return 0, err
	}
	// The shape is cached for the life of the connection, and a table whose
	// columns changed under it would otherwise keep the old one forever. A
	// failed load drops the entry, so the next one asks again rather than
	// failing the same way until the connection is recycled.
	defer func() {
		if c.bad {
			delete(c.bulk, key)
		}
	}()

	// Step 1: declare the shape. An ordinary batch, and the server answers it
	// before the rows may be sent — so the reply is read rather than assumed.
	var decl []byte
	decl = append(decl, "INSERT BULK "...)
	decl = append(decl, identBracket(table)...)
	decl = append(decl, " ("...)
	for i := range meta {
		if i > 0 {
			decl = append(decl, ", "...)
		}
		decl = append(decl, identBracket(cols[i])...)
		decl = append(decl, ' ')
		t, err := bulkTypeName(&meta[i])
		if err != nil {
			return 0, err
		}
		decl = append(decl, t...)
	}
	decl = append(decl, ')')
	if err := c.sendBatch(string(decl)); err != nil {
		c.bad = true
		return 0, err
	}
	if err := c.readUntilDone(); err != nil {
		c.bad = true
		return 0, err
	}

	// Step 2: the rows.
	x := c.x
	x.begin(pktBulkLoad)
	if err := c.writeBulkMetadata(meta, cols); err != nil {
		c.bad = true
		return 0, err
	}
	var n int64
	for src.Next() {
		v := src.Values()
		if len(v) != len(meta) {
			c.bad = true
			return 0, protoErr("a row has %d values for %d columns", len(v), len(meta))
		}
		if err := x.writeByte(tokRow); err != nil {
			c.bad = true
			return 0, err
		}
		for i := range meta {
			if err := c.writeBulkValue(&meta[i], v[i]); err != nil {
				c.bad = true
				return 0, err
			}
		}
		n++
	}
	if err := src.Err(); err != nil {
		c.bad = true
		return 0, err
	}
	// The terminator is a DONE token, not an end-of-packet: the server reads
	// rows until it sees one, and a stream that just stops leaves it waiting.
	done := []byte{tokDone, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if err := x.write(done); err != nil {
		c.bad = true
		return 0, err
	}
	if err := x.end(); err != nil {
		c.bad = true
		return 0, err
	}

	// Step 3: the count the server actually wrote, which is the one to report —
	// a row the source produced and the server rejected is not loaded.
	loaded, err := c.readCount()
	if err != nil {
		c.bad = true
		return 0, err
	}
	if loaded == 0 && n > 0 {
		// Some servers report the count on a DONE without the count bit. The
		// rows went in; reporting zero would make a caller think nothing did.
		loaded = n
	}
	return loaded, nil
}

// readUntilDone consumes a reply, returning the first error it carried.
func (c *Conn) readUntilDone() error {
	_, err := c.readReply()
	return err
}

// readCount consumes a reply and returns the rows it reports.
func (c *Conn) readCount() (int64, error) { return c.readReply() }

// readReply walks a reply's tokens, summing statement row counts and keeping
// the first error.
func (c *Conn) readReply() (int64, error) {
	var affected int64
	var first error
	for {
		t, err := c.x.readByte()
		if err != nil {
			if errIsEndOfMessage(err) {
				break
			}
			return 0, err
		}
		switch t {
		case tokDone, tokDoneProc, tokDoneInProc:
			st, rows, err := c.x.readDone()
			if err != nil {
				return 0, err
			}
			affected += countOf(t, st, rows)
		case tokError:
			e, err := c.x.readMessage()
			if err != nil {
				return 0, err
			}
			if first == nil {
				first = classify(e)
			}
		case tokInfo:
			if _, err := c.x.readMessage(); err != nil {
				return 0, err
			}
		case tokEnvChange:
			if err := c.x.readEnvChange(); err != nil {
				return 0, err
			}
		case tokReturnStatus:
			if _, err := c.x.readU32(); err != nil {
				return 0, err
			}
		case tokColMetadata:
			if _, err := c.x.readColMetadata(nil); err != nil {
				return 0, err
			}
		default:
			return 0, protoErr("unexpected token 0x%02X in a bulk reply", t)
		}
		if !c.x.inMsg {
			break
		}
	}
	return affected, first
}

// writeBulkMetadata writes the COLMETADATA the row stream is read against.
func (c *Conn) writeBulkMetadata(meta []column, cols []string) error {
	x := c.x
	// The TOKEN, then the count. The bulk stream is a token stream like any
	// other — COLMETADATA, ROW, ROW, …, DONE — and leaving the token off makes
	// the server read the column count as a token id and everything after it
	// one field out of step, which it reports as a premature end of message.
	if err := x.writeByte(tokColMetadata); err != nil {
		return err
	}
	if err := x.writeU16(uint16(len(meta))); err != nil {
		return err
	}
	for i := range meta {
		if err := x.writeU32(0); err != nil { // user type
			return err
		}
		// The server's own flags, echoed. Inventing them is what produced
		// "premature end-of-message": declaring a column nullable while sending
		// a non-nullable TYPE for it makes the server read a length byte that
		// is not there and then run out of row.
		if err := x.writeU16(meta[i].flags); err != nil {
			return err
		}
		if err := c.writeTypeInfo(&meta[i]); err != nil {
			return err
		}
		n := 0
		for range cols[i] {
			n++
		}
		if err := x.writeByte(byte(n)); err != nil {
			return err
		}
		if err := x.writeUCS2(cols[i]); err != nil {
			return err
		}
	}
	return nil
}

// writeTypeInfo echoes a column's TYPE_INFO back to the server.
//
// The mirror of readTypeInfo, and it has to stay one: a shape the reader can
// parse and the writer cannot produce is a load that fails on a table storm
// itself created.
func (c *Conn) writeTypeInfo(col *column) error {
	x := c.x
	if err := x.writeByte(col.id); err != nil {
		return err
	}
	switch col.id {
	case typeInt1, typeBit, typeInt2, typeInt4, typeFloat4, typeSmallDate,
		typeMoney4, typeInt8, typeFloat8, typeMoney, typeDateTime:
		return nil // fixed width, nothing follows

	case typeGUID, typeIntN, typeBitN, typeFloatN, typeMoneyN, typeDateTimeN:
		// The width the SERVER declared, not a guess. An INT arrives as INTN
		// with a declared width of four, and echoing eight makes the stream
		// disagree with both the declaration and the table.
		return x.writeByte(byte(col.size))

	case typeDecimal, typeNumeric, typeDecimalN, typeNumericN:
		if err := x.writeByte(byte(col.size)); err != nil {
			return err
		}
		if err := x.writeByte(col.prec); err != nil {
			return err
		}
		return x.writeByte(col.scale)

	case typeDate:
		return nil

	case typeTime, typeDateTime2, typeDateTimeOff:
		return x.writeByte(col.scale)

	case typeBigVarBin, typeBigBinary:
		return x.writeU16(uint16(col.size))

	case typeBigVarChar, typeBigChar, typeNVarChar, typeNChar:
		if err := x.writeU16(uint16(col.size)); err != nil {
			return err
		}
		return x.write(col.collation[:])
	}
	return protoErr("a bulk load cannot send TDS type 0x%02X — CAST the column, or "+
		"insert it row by row", col.id)
}

// bulkTypeName is the type as INSERT BULK's declaration spells it.
func bulkTypeName(col *column) (string, error) {
	switch col.id {
	case typeBit, typeBitN:
		return "bit", nil
	case typeInt1:
		return "tinyint", nil
	case typeInt2:
		return "smallint", nil
	case typeInt4:
		return "int", nil
	case typeInt8:
		return "bigint", nil
	case typeIntN:
		switch col.size {
		case 1:
			return "tinyint", nil
		case 2:
			return "smallint", nil
		case 4:
			return "int", nil
		}
		return "bigint", nil
	case typeFloat4:
		return "real", nil
	case typeFloat8:
		return "float(53)", nil
	case typeFloatN:
		if col.size == 4 {
			return "real", nil
		}
		return "float(53)", nil
	case typeGUID:
		return "uniqueidentifier", nil
	case typeDecimal, typeNumeric, typeDecimalN, typeNumericN:
		return "decimal(" + strconv.Itoa(int(col.prec)) + "," +
			strconv.Itoa(int(col.scale)) + ")", nil
	case typeDate:
		return "date", nil
	case typeTime:
		return "time(" + strconv.Itoa(int(col.scale)) + ")", nil
	case typeDateTime2:
		return "datetime2(" + strconv.Itoa(int(col.scale)) + ")", nil
	case typeDateTimeOff:
		return "datetimeoffset(" + strconv.Itoa(int(col.scale)) + ")", nil
	case typeDateTime, typeDateTimeN:
		return "datetime", nil
	case typeNVarChar, typeNChar:
		return sized("nvarchar", col.size/2, col.plp), nil
	case typeBigVarChar, typeBigChar:
		return sized("varchar", col.size, col.plp), nil
	case typeBigVarBin, typeBigBinary:
		return sized("varbinary", col.size, col.plp), nil
	}
	return "", protoErr("a bulk load cannot declare TDS type 0x%02X", col.id)
}

// sized renders a width the way a declaration spells it.
func sized(name string, n int, plp bool) string {
	if plp || n <= 0 {
		return name + "(max)"
	}
	return name + "(" + strconv.Itoa(n) + ")"
}

// writeBulkValue encodes one value in the COLUMN's type.
//
// In the column's type, not the value's: a bulk stream carries no parameter
// declaration, so there is no conversion step and a value written in the wrong
// width is read as the next column's bytes. That is why bulkMeta asks the
// server rather than inferring from Go types.
func (c *Conn) writeBulkValue(col *column, v any) error {
	x := c.x
	if v == nil || isNilPtr(v) {
		return c.writeBulkNull(col)
	}
	switch col.id {
	case typeBit, typeBitN:
		b := byte(0)
		if truthy(v) {
			b = 1
		}
		if col.id == typeBit {
			// FIXED, so no length byte. Writing one anyway adds a byte to every
			// row, and the server reports it as the NEXT unicode column having
			// an odd byte size — a true statement about the wrong column, and
			// the reason this case is spelled out rather than shared with the
			// nullable one.
			return x.write([]byte{b})
		}
		return x.write([]byte{1, b})

	case typeInt1, typeInt2, typeInt4, typeInt8, typeIntN:
		n, err := asInt(v)
		if err != nil {
			return err
		}
		w := intWidth(col)
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(n))
		if col.id == typeIntN {
			if err := x.writeByte(byte(w)); err != nil {
				return err
			}
		}
		return x.write(buf[:w])

	case typeFloat4:
		f, err := asFloat(v)
		if err != nil {
			return err
		}
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], math.Float32bits(float32(f)))
		return x.write(buf[:])

	case typeFloat8, typeFloatN:
		f, err := asFloat(v)
		if err != nil {
			return err
		}
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(f))
		if col.id == typeFloatN {
			if col.size == 4 {
				var b4 [4]byte
				binary.LittleEndian.PutUint32(b4[:], math.Float32bits(float32(f)))
				if err := x.writeByte(4); err != nil {
					return err
				}
				return x.write(b4[:])
			}
			if err := x.writeByte(8); err != nil {
				return err
			}
		}
		return x.write(buf[:])

	case typeGUID:
		u, ok := asUUID(v)
		if !ok {
			return protoErr("a uniqueidentifier column was given a %T", v)
		}
		var b [16]byte
		b[0], b[1], b[2], b[3] = u[3], u[2], u[1], u[0]
		b[4], b[5] = u[5], u[4]
		b[6], b[7] = u[7], u[6]
		copy(b[8:], u[8:])
		if err := x.writeByte(16); err != nil {
			return err
		}
		return x.write(b[:])

	case typeDecimal, typeNumeric, typeDecimalN, typeNumericN:
		d, ok := v.(runtime.Decimal)
		if !ok {
			return protoErr("a decimal column was given a %T", v)
		}
		return c.writeBulkDecimal(col, d)

	case typeDate, typeTime, typeDateTime2, typeDateTimeOff:
		return c.writeBulkTemporal(col, v)

	case typeNVarChar, typeNChar:
		s, err := asString(v)
		if err != nil {
			return err
		}
		if col.plp {
			return x.writeBulkPLP([]byte(nil), s)
		}
		if err := x.writeU16(uint16(ucs2Len(s))); err != nil {
			return err
		}
		return x.writeUCS2(s)

	case typeBigVarBin, typeBigBinary:
		b, ok := asBytes(v)
		if !ok {
			return protoErr("a varbinary column was given a %T", v)
		}
		if col.plp {
			return x.writeBulkPLP(b, "")
		}
		if err := x.writeU16(uint16(len(b))); err != nil {
			return err
		}
		return x.write(b)
	}
	return protoErr("a bulk load cannot send TDS type 0x%02X", col.id)
}

// writeBulkPLP writes a MAX value with the UNKNOWN-length header.
//
// Not the known-length form the parameter path uses. With a declared total the
// server reads exactly that many bytes of chunks and stops, so the terminator
// that follows is read as the next row's token — and the row after it runs out
// four bytes early, which the server reports as a premature end of message.
// The unknown-length header makes the terminator the only thing that ends the
// value, which is unambiguous in both directions.
func (x *conn) writeBulkPLP(b []byte, s string) error {
	if err := x.writeU64(plpUnknown); err != nil {
		return err
	}
	n := len(b)
	if b == nil {
		n = ucs2Len(s)
	}
	if n > 0 {
		if err := x.writeU32(uint32(n)); err != nil {
			return err
		}
		if b != nil {
			if err := x.write(b); err != nil {
				return err
			}
		} else if err := x.writeUCS2(s); err != nil {
			return err
		}
	}
	return x.writeU32(0)
}

// writeBulkNull writes the null form for a column's type.
func (c *Conn) writeBulkNull(col *column) error {
	x := c.x
	switch col.id {
	case typeNVarChar, typeNChar, typeBigVarChar, typeBigChar,
		typeBigVarBin, typeBigBinary:
		if col.plp {
			return x.writeU64(plpNull)
		}
		return x.writeU16(0xFFFF)
	}
	// Every other type this loader sends is length-prefixed with one byte, and
	// a length of zero is its NULL.
	return x.writeByte(0)
}

// writeBulkDecimal writes sign and magnitude at the column's declared scale.
func (c *Conn) writeBulkDecimal(col *column, d runtime.Decimal) error {
	// Rescaled to the COLUMN's scale, because the stream carries no scale of
	// its own: a value at scale 2 written into a scale-4 column is a hundred
	// times too small, silently.
	u := d.Unscaled
	for s := d.Scale; s < int32(col.scale); s++ {
		u *= 10
	}
	for s := d.Scale; s > int32(col.scale); s-- {
		u /= 10
	}
	sign := byte(1)
	if u < 0 {
		sign = 0
		u = -u
	}
	// The COLUMN's declared width, not the widest form. A decimal's wire size
	// follows its precision — five bytes up to 9 digits, nine up to 19, and so
	// on — and writing seventeen into a column that declared nine leaves eight
	// bytes of magnitude sitting where the next column's value should be. The
	// server reports that as the column AFTER it having an odd byte size, which
	// is a true statement about the wrong column.
	n := col.size
	if n < 5 || n > 17 {
		n = 17
	}
	var b [17]byte
	b[0] = sign
	binary.LittleEndian.PutUint64(b[1:], uint64(u))
	if err := c.x.writeByte(byte(n)); err != nil {
		return err
	}
	return c.x.write(b[:n])
}

// writeBulkTemporal writes a date, time, datetime2 or datetimeoffset at the
// column's declared scale.
func (c *Conn) writeBulkTemporal(col *column, v any) error {
	x := c.x
	switch col.id {
	case typeTime:
		d, ok := v.(runtime.TimeOfDay)
		if !ok {
			t, err := asTime(v)
			if err != nil {
				return err
			}
			d = runtime.TimeOfDay((int64(t.Hour())*3600 + int64(t.Minute())*60 +
				int64(t.Second())) * 1000000)
		}
		ticks := uint64(d.Duration() / 100)
		w := timeWidth(col.scale)
		ticks = rescaleDown(ticks, col.scale)
		if err := x.writeByte(byte(w)); err != nil {
			return err
		}
		return x.write(leBytes(ticks, w))

	case typeDate:
		t, err := asTime(v)
		if err != nil {
			return err
		}
		if err := x.writeByte(3); err != nil {
			return err
		}
		return x.write(leBytes(uint64(dayNumber(t)), 3))

	case typeDateTime2, typeDateTimeOff:
		t, err := asTime(v)
		if err != nil {
			return err
		}
		u := t.UTC()
		ticks := uint64(u.Hour())*3600 + uint64(u.Minute())*60 + uint64(u.Second())
		ticks = ticks*10000000 + uint64(u.Nanosecond())/100
		ticks = rescaleDown(ticks, col.scale)
		w := timeWidth(col.scale)
		n := w + 3
		if col.id == typeDateTimeOff {
			n += 2
		}
		if err := x.writeByte(byte(n)); err != nil {
			return err
		}
		if err := x.write(leBytes(ticks, w)); err != nil {
			return err
		}
		if err := x.write(leBytes(uint64(dayNumber(u)), 3)); err != nil {
			return err
		}
		if col.id == typeDateTimeOff {
			_, off := t.Zone()
			return x.write(leBytes(uint64(uint16(int16(off/60))), 2))
		}
		return nil
	}
	return protoErr("not a temporal type: 0x%02X", col.id)
}

// rescaleDown converts hundred-nanosecond ticks to the column's scale.
func rescaleDown(ticks uint64, scale byte) uint64 {
	for s := scale; s < 7; s++ {
		ticks /= 10
	}
	return ticks
}

// dayNumber is days since 0001-01-01, which date and datetime2 count from.
func dayNumber(t time.Time) uint32 {
	u := t.UTC()
	d := u.Unix() / 86400
	if u.Unix() < 0 && u.Unix()%86400 != 0 {
		d--
	}
	return uint32(d) + daysToEpoch
}

func leBytes(v uint64, n int) []byte {
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = byte(v >> (8 * i))
	}
	return b
}

func intWidth(col *column) int {
	switch col.id {
	case typeInt1:
		return 1
	case typeInt2:
		return 2
	case typeInt4:
		return 4
	case typeIntN:
		return col.size
	}
	return 8
}

// The value adapters. A CopySource hands back `any`, so the same Go value may
// arrive as a pointer or not — storm's binder passes pointers into its own
// reusable buffers, and a nil one is how a NULL arrives.

func isNilPtr(v any) bool {
	switch p := v.(type) {
	case *int64:
		return p == nil
	case *int32:
		return p == nil
	case *int16:
		return p == nil
	case *int8:
		return p == nil
	case *int:
		return p == nil
	case *bool:
		return p == nil
	case *string:
		return p == nil
	case *float64:
		return p == nil
	case *float32:
		return p == nil
	case *time.Time:
		return p == nil
	case *[]byte:
		return p == nil || *p == nil
	case []byte:
		return p == nil
	}
	return false
}

func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case *bool:
		return b != nil && *b
	}
	return false
}

func asInt(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int32:
		return int64(n), nil
	case int16:
		return int64(n), nil
	case int8:
		return int64(n), nil
	case int:
		return int64(n), nil
	case uint64:
		return int64(n), nil
	case *int64:
		return *n, nil
	case *int32:
		return int64(*n), nil
	case *int16:
		return int64(*n), nil
	case *int8:
		return int64(*n), nil
	case *int:
		return int64(*n), nil
	}
	return 0, protoErr("an integer column was given a %T", v)
}

func asFloat(v any) (float64, error) {
	switch f := v.(type) {
	case float64:
		return f, nil
	case float32:
		return float64(f), nil
	case *float64:
		return *f, nil
	case *float32:
		return float64(*f), nil
	}
	return 0, protoErr("a float column was given a %T", v)
}

func asString(v any) (string, error) {
	switch s := v.(type) {
	case string:
		return s, nil
	case *string:
		return *s, nil
	case []byte:
		return string(s), nil
	}
	if sr, ok := v.(interface{ String() string }); ok {
		return sr.String(), nil
	}
	return "", protoErr("a text column was given a %T", v)
}

func asBytes(v any) ([]byte, bool) {
	switch b := v.(type) {
	case []byte:
		return b, true
	case *[]byte:
		return *b, true
	case [16]byte:
		return b[:], true
	}
	return nil, false
}

func asUUID(v any) ([16]byte, bool) {
	switch u := v.(type) {
	case [16]byte:
		return u, true
	case *[16]byte:
		return *u, true
	case []byte:
		if len(u) == 16 {
			var a [16]byte
			copy(a[:], u)
			return a, true
		}
	}
	return [16]byte{}, false
}

func asTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t, nil
	case *time.Time:
		return *t, nil
	}
	return time.Time{}, protoErr("a temporal column was given a %T", v)
}
