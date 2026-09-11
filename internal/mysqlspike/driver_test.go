package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"testing"

	mysqldrv "github.com/go-sql-driver/mysql"
)

const dsn = "root:storm@tcp(192.168.64.3:3306)/storm_m9"

// Eight columns, the width bench/ uses for a row read.
const q = "SELECT `id`,`email`,`name`,`age`,`org_id`,`id`,`age`,`org_id` FROM `spike_big` LIMIT 200"

// Through database/sql, which is what an adapter built the easy way would do.
func BenchmarkDatabaseSQLScan(b *testing.B) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	var i1, i5, i7 int64
	var s1, s2 string
	var n1, n2, n3 sql.NullInt64
	b.ReportAllocs()
	b.ResetTimer()
	rows := 0
	for n := 0; n < b.N; n++ {
		r, err := db.Query(q)
		if err != nil {
			b.Fatal(err)
		}
		for r.Next() {
			if err := r.Scan(&i1, &s1, &s2, &n1, &i5, &i7, &n2, &n3); err != nil {
				b.Fatal(err)
			}
			rows++
		}
		r.Close()
	}
	b.ReportMetric(float64(rows)/float64(b.N), "rows/op")
}

// Through driver.Rows directly, bypassing database/sql entirely. This is the
// FLOOR for an adapter on top of this driver: no sql.Rows, no Scan, no
// reflection — just the values the driver hands back.
func BenchmarkDriverRowsNext(b *testing.B) {
	c, err := mysqldrv.MySQLDriver{}.Open(dsn)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	qc := c.(driver.QueryerContext)
	dest := make([]driver.Value, 8)
	b.ReportAllocs()
	b.ResetTimer()
	rows := 0
	for n := 0; n < b.N; n++ {
		r, err := qc.QueryContext(context.Background(), q, nil)
		if err != nil {
			b.Fatal(err)
		}
		for {
			if err := r.Next(dest); err == io.EOF {
				break
			} else if err != nil {
				b.Fatal(err)
			}
			rows++
		}
		r.Close()
	}
	b.ReportMetric(float64(rows)/float64(b.N), "rows/op")
}

// What the driver actually puts in a driver.Value, which decides whether a
// zero-copy RawValues() is reachable at all.
func TestWhatTheDriverHandsBack(t *testing.T) {
	c, err := mysqldrv.MySQLDriver{}.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := c.(driver.QueryerContext).QueryContext(context.Background(),
		"SELECT `id`,`email`,`age` FROM `spike_big` LIMIT 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dest := make([]driver.Value, 3)
	if err := r.Next(dest); err != nil {
		t.Fatal(err)
	}
	for i, v := range dest {
		t.Logf("col %d: %T = %v", i, v, v)
	}
}
