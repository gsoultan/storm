package msbench

// What a database/sql SQL Server driver costs per row.
//
// ADR-0007's argument for a hand-written MySQL client was that every Go MySQL
// driver decodes into driver.Value before storm can see the bytes, costing one
// boxing allocation per column per row. The same question has to be asked of
// SQL Server before M10 commits to an approach, and the answer decides whether
// the milestone is a lowering or a lowering plus a TDS client.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"os"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

const dsnEnv = "STORM_MSSQL_DSN"

func seed(t testing.TB) string {
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skip(dsnEnv + " unset")
	}
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TABLE IF EXISTS bench_probe`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE bench_probe (
		c0 BIGINT NOT NULL, c1 BIGINT NOT NULL, c2 BIGINT NOT NULL, c3 BIGINT NOT NULL,
		c4 BIGINT NOT NULL, c5 BIGINT NOT NULL, c6 BIGINT NOT NULL, c7 BIGINT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		b := int64(1_000_000 + i*8)
		if _, err := db.Exec(
			`INSERT INTO bench_probe VALUES (@p1,@p2,@p3,@p4,@p5,@p6,@p7,@p8)`,
			b, b+1, b+2, b+3, b+4, b+5, b+6, b+7); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

// Through driver.Rows DIRECTLY, bypassing database/sql — the same measurement
// internal/mysqlspike took, and the most favourable one the library can give.
func BenchmarkDriverRows200x8(b *testing.B) {
	dsn := seed(b)
	c, err := mssql.NewConnector(dsn)
	if err != nil {
		b.Fatal(err)
	}
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
