package orabench

// Result 2: what a database/sql Oracle driver costs per row.
//
// ADR-0007's argument for a hand-written MySQL client was that every Go MySQL
// driver decodes into driver.Value before storm can see the bytes, costing one
// boxing allocation per column per row. M10 asked the same of SQL Server and
// got 11.3 allocations per row against go-mssqldb, which is what bought
// runtime/msdrv. Asking it a third time is the plan's own rule: the driver
// decision is the estimate to make before starting, not after.
//
// Measured the most FAVOURABLE way for the library — through driver.Rows
// directly, bypassing database/sql, exactly as internal/mysqlspike and
// internal/mssqlspike did. A number taken through database/sql would be higher
// and would be measuring the standard library rather than the driver.

import (
	"context"
	"database/sql/driver"
	"io"
	"os"
	"testing"

	go_ora "github.com/sijms/go-ora/v2"
)

const benchRows = 200

func seed(b testing.TB) string {
	db := open(b)
	drop(db, "TABLE", "bench_probe PURGE")
	mustExec(b, db, `CREATE TABLE bench_probe (
		c0 NUMBER(19) NOT NULL, c1 NUMBER(19) NOT NULL, c2 NUMBER(19) NOT NULL,
		c3 NUMBER(19) NOT NULL, c4 NUMBER(19) NOT NULL, c5 NUMBER(19) NOT NULL,
		c6 NUMBER(19) NOT NULL, c7 NUMBER(19) NOT NULL)`)
	for i := 0; i < benchRows; i++ {
		v := int64(1_000_000 + i*8)
		mustExec(b, db,
			`INSERT INTO bench_probe VALUES (:1,:2,:3,:4,:5,:6,:7,:8)`,
			v, v+1, v+2, v+3, v+4, v+5, v+6, v+7)
	}
	return os.Getenv(dsnEnv)
}

// BenchmarkDriverRows200x8 is the number. Divide allocs/op by rows/op.
func BenchmarkDriverRows200x8(b *testing.B) {
	dsn := seed(b)
	c := go_ora.NewConnector(dsn)
	conn, err := c.Connect(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	st, err := conn.Prepare("SELECT c0,c1,c2,c3,c4,c5,c6,c7 FROM bench_probe")
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()

	b.ReportAllocs()
	b.ResetTimer()
	rows := 0
	dest := make([]driver.Value, 8)
	for i := 0; i < b.N; i++ {
		r, err := st.Query(nil)
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
	b.StopTimer()
	b.ReportMetric(float64(rows)/float64(b.N), "rows/op")
}

// And the same rows through database/sql, which is what an application that
// reached for the library directly would actually pay. The gap between the two
// is the standard library's share, and it is not storm's to remove.
func BenchmarkDatabaseSQLRows200x8(b *testing.B) {
	seed(b)
	db := open(b)

	b.ReportAllocs()
	b.ResetTimer()
	rows := 0
	var v [8]int64
	for i := 0; i < b.N; i++ {
		rs, err := db.Query("SELECT c0,c1,c2,c3,c4,c5,c6,c7 FROM bench_probe")
		if err != nil {
			b.Fatal(err)
		}
		for rs.Next() {
			if err := rs.Scan(&v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7]); err != nil {
				b.Fatal(err)
			}
			rows++
		}
		rs.Close()
	}
	b.StopTimer()
	b.ReportMetric(float64(rows)/float64(b.N), "rows/op")
}
