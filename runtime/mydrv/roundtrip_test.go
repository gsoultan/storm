package mydrv_test

// Every column type storm supports, written through the binder and read back
// through the decoder, against a real server.
//
// The unit tests on either side both pass with a contract mismatch between
// them: mydec's build hand-writes the bytes it expects, and the binder's tests
// check what it produced. The one bug that shape hides is the one that already
// happened — temporals whose length prefix one side stripped and the other
// side read as a component count. Only the server, sitting in the middle, can
// tell them apart.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/mydec"
	"github.com/gsoultan/storm/runtime/mydrv"
)

func TestEveryTypeRoundTrips(t *testing.T) {
	for _, engine := range []struct {
		name string
		cfg  func(testing.TB) mydrv.Config
	}{{"mysql", config}, {"mariadb", mariaConfig}} {
		t.Run(engine.name, func(t *testing.T) {
			c, err := mydrv.Open(context.Background(), engine.cfg(t))
			if err != nil {
				t.Skipf("no server: %v", err)
			}
			defer c.Close()
			roundTrips(t, c)
		})
	}
}

func roundTrips(t *testing.T, c *mydrv.Conn) {
	ctx := context.Background()
	mustExec(t, c, "DROP TABLE IF EXISTS `rt_probe`")
	mustExec(t, c, "CREATE TABLE `rt_probe` ("+
		"`b` TINYINT(1) NOT NULL,"+
		"`i2` SMALLINT NOT NULL,"+
		"`i4` INT NOT NULL,"+
		"`i8` BIGINT NOT NULL,"+
		"`f4` FLOAT NOT NULL,"+
		"`f8` DOUBLE NOT NULL,"+
		"`s` VARCHAR(80) NOT NULL,"+
		"`bin` VARBINARY(40) NOT NULL,"+
		"`u` BINARY(16) NOT NULL,"+
		"`ts` DATETIME(6) NOT NULL,"+
		"`d` DATE NOT NULL,"+
		"`tod` TIME(6) NOT NULL,"+
		"`dec` DECIMAL(18,6) NOT NULL,"+
		"`js` JSON NOT NULL,"+
		"`en` ENUM('draft','live') NOT NULL,"+
		"`nul` VARCHAR(20)) ENGINE=InnoDB")
	t.Cleanup(func() { _, _ = c.Exec(ctx, "DROP TABLE IF EXISTS `rt_probe`", nil) })

	var (
		wantUUID = [16]byte{0xfe, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 0xef}
		wantTS   = time.Date(2026, 9, 12, 23, 59, 58, 123456000, time.UTC)
		wantDate = time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC)
		// Over a day and negative: MySQL's TIME is a signed duration, not a
		// clock reading, and a decoder that treats it as one loses both facts.
		wantTOD = -(30*time.Hour + 20*time.Minute + 10*time.Second + 500000*time.Microsecond)
	)
	// 18 significant digits is what a storm Decimal holds; this sits at the
	// edge of it rather than over, so the test is about the round trip and not
	// about the type's range.
	wantDec, err := runtime.ParseDecimal("-123456789012.345678")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx,
		"INSERT INTO `rt_probe` VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)",
		[]any{
			true, int16(-32768), int32(2147483647), int64(-9223372036854775808),
			float32(0.5), float64(-1.25),
			"héllo — ünicode", []byte{0, 1, 2, 0xff}, wantUUID,
			wantTS, wantDate, wantTOD, wantDec,
			`{"a":1}`, "live",
		}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	r, err := c.Query(ctx, "SELECT `b`,`i2`,`i4`,`i8`,`f4`,`f8`,`s`,`bin`,`u`,"+
		"`ts`,`d`,`tod`,`dec`,`js`,`en`,`nul` FROM `rt_probe`", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if !r.Next() {
		t.Fatalf("no row: %v", r.Err())
	}
	v := r.RawValues()

	if got := mydec.Bool(v[0]); got != true {
		t.Errorf("bool = %v", got)
	}
	if got := mydec.Int2(v[1]); got != -32768 {
		t.Errorf("int2 = %d", got)
	}
	if got := mydec.Int4(v[2]); got != 2147483647 {
		t.Errorf("int4 = %d", got)
	}
	if got := mydec.Int8(v[3]); got != -9223372036854775808 {
		t.Errorf("int8 = %d", got)
	}
	if got := mydec.Float4(v[4]); got != 0.5 {
		t.Errorf("float4 = %v", got)
	}
	if got := mydec.Float8(v[5]); got != -1.25 {
		t.Errorf("float8 = %v", got)
	}
	if got := mydec.Text(v[6]); got != "héllo — ünicode" {
		t.Errorf("text = %q", got)
	}
	if got := mydec.Bytes(v[7]); !bytes.Equal(got, []byte{0, 1, 2, 0xff}) {
		t.Errorf("bytes = %v", got)
	}
	if got := mydec.UUID(v[8]); got != wantUUID {
		t.Errorf("uuid = %x, want %x", got, wantUUID)
	}
	if got, err := mydec.DateTime(v[9]); err != nil {
		t.Errorf("datetime: %v", err)
	} else if !got.Equal(wantTS) {
		t.Errorf("datetime = %v, want %v", got, wantTS)
	}
	if got, err := mydec.Date(v[10]); err != nil {
		t.Errorf("date: %v", err)
	} else if !got.Equal(wantDate) {
		t.Errorf("date = %v, want %v", got, wantDate)
	}
	if got, err := mydec.Duration(v[11]); err != nil {
		t.Errorf("duration: %v", err)
	} else if got != wantTOD {
		t.Errorf("duration = %v, want %v", got, wantTOD)
	}
	if got, err := mydec.Decimal(v[12]); err != nil {
		t.Errorf("decimal: %v", err)
	} else if got.String() != wantDec.String() {
		t.Errorf("decimal = %s, want %s", got, wantDec)
	}
	// JSON arrives as its text. A driver that handed back MySQL's internal
	// binary JSON would give the scanners a document they cannot read.
	if got := mydec.NullJSON(v[13], &runtime.Slab{}); !got.Valid ||
		string(got.V) != `{"a": 1}` && string(got.V) != `{"a":1}` {
		t.Errorf("json = %q valid=%v", got.V, got.Valid)
	}
	// An enum's LABEL is the value, which is why a storm enum scans into a
	// string with no lookup table.
	if got := mydec.Text(v[14]); got != "live" {
		t.Errorf("enum = %q", got)
	}
	// A NULL column must arrive as a nil slice, not as an empty one: an empty
	// string and a NULL are different rows, and the scanners tell them apart
	// by exactly this.
	if v[15] != nil {
		t.Errorf("the NULL column decoded to %q rather than nil", v[15])
	}
	// ...and the Null decoders have to agree, on the same bytes.
	if got := mydec.NullText(v[15], &runtime.Slab{}); got.Valid {
		t.Errorf("NullText called a NULL valid: %+v", got)
	}
	if got, err := mydec.NullDateTime(v[15]); err != nil {
		t.Errorf("NullDateTime on NULL: %v", err)
	} else if got.Valid {
		t.Error("NullDateTime called a NULL valid")
	}
	if got, err := mydec.NullNumeric(v[15]); err != nil {
		t.Errorf("NullNumeric on NULL: %v", err)
	} else if got.Valid {
		t.Error("NullNumeric called a NULL valid")
	}
	if got, err := mydec.NullDuration(v[15]); err != nil {
		t.Errorf("NullDuration on NULL: %v", err)
	} else if got.Valid {
		t.Error("NullDuration called a NULL valid")
	}
	if r.Next() {
		t.Error("more than one row")
	}
}
