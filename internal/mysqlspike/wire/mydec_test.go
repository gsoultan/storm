package main

import (
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime/mydec"
)

// ADR-0007's premise, tested against a server for the first time.
//
// The seam has a second decoder family because "MySQL is little-endian where
// PostgreSQL is big-endian, and packs its temporal types component-wise rather
// than as an epoch offset". Nothing had ever handed mydec bytes off a wire —
// the MySQL package compiled, and compiling is not decoding.
func TestMydecDecodesRealBinaryProtocolBytes(t *testing.T) {
	c, err := dial(addr, "root", "storm", "storm")
	if err != nil {
		t.Skipf("no MariaDB: %v", err)
	}
	defer c.c.Close()

	mustExec(t, c, "DROP TABLE IF EXISTS `dec_probe`")
	mustExec(t, c, "CREATE TABLE `dec_probe` (`i8` BIGINT, `i4` INT, `i2` SMALLINT, "+
		"`b` TINYINT(1), `s` VARCHAR(40), `d` DECIMAL(18,4), `ts` DATETIME(6), `dt` DATE)")
	mustExec(t, c, "INSERT INTO `dec_probe` VALUES "+
		"(9223372036854775807, -2147483648, -32768, 1, 'héllo', '12345.6789', "+
		"'2026-09-12 11:22:33.456789', '2026-09-12')")

	s, err := c.prepare("SELECT `i8`,`i4`,`i2`,`b`,`s`,`d`,`ts`,`dt` FROM `dec_probe`")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	rows := 0
	err = s.exec(nil, func(cols [][]byte) error {
		rows++
		if got := mydec.Int8(cols[0]); got != 9223372036854775807 {
			t.Errorf("Int8 = %d, want max int64 — a byte-order bug reads this as garbage", got)
		}
		if got := mydec.Int4(cols[1]); got != -2147483648 {
			t.Errorf("Int4 = %d, want min int32", got)
		}
		if got := mydec.Int2(cols[2]); got != -32768 {
			t.Errorf("Int2 = %d, want min int16", got)
		}
		if !mydec.Bool(cols[3]) {
			t.Error("Bool decoded false")
		}
		if got := mydec.Text(cols[4]); got != "héllo" {
			t.Errorf("Text = %q", got)
		}
		dec, err := mydec.Decimal(cols[5])
		if err != nil {
			t.Errorf("Decimal: %v", err)
		} else if dec.String() != "12345.6789" {
			t.Errorf("Decimal = %s, want 12345.6789", dec.String())
		}
		ts, err := mydec.DateTime(cols[6])
		if err != nil {
			t.Errorf("DateTime: %v", err)
		} else {
			want := time.Date(2026, 9, 12, 11, 22, 33, 456789000, time.UTC)
			if !ts.Equal(want) {
				t.Errorf("DateTime = %v, want %v", ts, want)
			}
		}
		d, err := mydec.Date(cols[7])
		if err != nil {
			t.Errorf("Date: %v", err)
		} else if d.Format("2006-01-02") != "2026-09-12" {
			t.Errorf("Date = %v", d)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("read %d rows, want 1", rows)
	}
}

func mustExec(t *testing.T, c *conn, sql string) {
	t.Helper()
	if err := c.query(sql, func([][]byte) error { return nil }); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// The prepared path is what a driver would actually use, so its profile is the
// one that matters — the COM_QUERY number was only ever a proxy.
func BenchmarkBinaryRowsPrepared(b *testing.B) {
	c, err := dial(addr, "root", "storm", "storm")
	if err != nil {
		b.Skipf("no MariaDB: %v", err)
	}
	defer c.c.Close()
	s, err := c.prepare("SELECT `id`,`email`,`org_id`,`id`,`email`,`org_id`,`id`,`org_id` " +
		"FROM `mc_rows` LIMIT 200")
	if err != nil {
		b.Fatal(err)
	}
	defer s.close()
	var sink int
	b.ReportAllocs()
	b.ResetTimer()
	rows := 0
	for i := 0; i < b.N; i++ {
		if err := s.exec(nil, func(cols [][]byte) error {
			rows++
			sink += len(cols[0])
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
	_ = sink
	b.ReportMetric(float64(rows)/float64(b.N), "rows/op")
}

// And decoding through mydec on top, which is the whole read path a generated
// scanner walks.
func BenchmarkBinaryRowsDecoded(b *testing.B) {
	c, err := dial(addr, "root", "storm", "storm")
	if err != nil {
		b.Skipf("no MariaDB: %v", err)
	}
	defer c.c.Close()
	s, err := c.prepare("SELECT `id`,`email`,`org_id` FROM `mc_rows` LIMIT 200")
	if err != nil {
		b.Fatal(err)
	}
	defer s.close()
	var n int64
	b.ReportAllocs()
	b.ResetTimer()
	rows := 0
	for i := 0; i < b.N; i++ {
		if err := s.exec(nil, func(cols [][]byte) error {
			rows++
			n += mydec.Int8(cols[0]) + mydec.Int8(cols[2])
			n += int64(len(mydec.Text(cols[1])))
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
	_ = n
	b.ReportMetric(float64(rows)/float64(b.N), "rows/op")
}
