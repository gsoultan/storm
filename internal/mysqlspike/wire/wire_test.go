package main

import "testing"

const addr = "192.168.64.188:3306"

func TestWireClientReadsRows(t *testing.T) {
	c, err := dial(addr, "root", "storm", "storm")
	if err != nil {
		t.Skipf("no MariaDB: %v", err)
	}
	defer c.c.Close()
	n := 0
	err = c.query("SELECT `id`,`email`,`org_id` FROM `mc_rows` LIMIT 3", func(cols [][]byte) error {
		if n == 0 {
			for i, v := range cols {
				t.Logf("col %d raw=%q", i, v)
			}
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("read %d rows, want 3", n)
	}
}

// The question the whole spike exists to answer.
func BenchmarkWireRawRows(b *testing.B) {
	c, err := dial(addr, "root", "storm", "storm")
	if err != nil {
		b.Skipf("no MariaDB: %v", err)
	}
	defer c.c.Close()
	const q = "SELECT `id`,`email`,`org_id`,`id`,`email`,`org_id`,`id`,`org_id` FROM `mc_rows` LIMIT 200"
	var sink int
	b.ReportAllocs()
	b.ResetTimer()
	rows := 0
	for i := 0; i < b.N; i++ {
		if err := c.query(q, func(cols [][]byte) error {
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
