package msdrv_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/gsoultan/storm/runtime/msdrv"
)

// The measurement this package exists for.
//
// internal/mssqlspike measured microsoft/go-mssqldb at 11.3 allocations per row
// through the most favourable path it offers — every column boxed into a
// driver.Value before storm can see a byte. This is the same shape of result,
// 200 rows of 8 columns, through the port storm actually uses.
func BenchmarkRows200x8(b *testing.B) {
	c, err := msdrv.Open(context.Background(), benchCfg(b))
	if err != nil {
		b.Skipf("no server: %v", err)
	}
	defer c.Close()
	ctx := context.Background()

	if _, err := c.Exec(ctx, `IF OBJECT_ID('dbo.msdrv_bench') IS NOT NULL DROP TABLE dbo.msdrv_bench;
		CREATE TABLE dbo.msdrv_bench (
			id INT NOT NULL PRIMARY KEY, a BIGINT NOT NULL, bb BIGINT NOT NULL,
			c NVARCHAR(40) NOT NULL, d NVARCHAR(40) NOT NULL,
			e BIGINT NOT NULL, f BIGINT NOT NULL, g NVARCHAR(40) NOT NULL)`, nil); err != nil {
		b.Fatal(err)
	}
	defer c.Exec(context.Background(), "DROP TABLE dbo.msdrv_bench", nil)

	var vals []string
	for i := 0; i < 200; i++ {
		vals = append(vals, fmt.Sprintf("(%d,%d,%d,'row%d','txt%d',%d,%d,'tail%d')",
			i, i*2, i*3, i, i, i*4, i*5, i))
	}
	if _, err := c.Exec(ctx,
		"INSERT INTO dbo.msdrv_bench (id,a,bb,c,d,e,f,g) VALUES "+strings.Join(vals, ","),
		nil); err != nil {
		b.Fatal(err)
	}

	const q = "SELECT id,a,bb,c,d,e,f,g FROM dbo.msdrv_bench ORDER BY id"
	b.ReportAllocs()
	b.ResetTimer()
	rows := 0
	for i := 0; i < b.N; i++ {
		r, err := c.Query(ctx, q, nil)
		if err != nil {
			b.Fatal(err)
		}
		for r.Next() {
			_ = r.RawValues()
			rows++
		}
		if err := r.Err(); err != nil {
			b.Fatal(err)
		}
		r.Close()
	}
	b.StopTimer()
	b.ReportMetric(float64(rows)/float64(b.N), "rows/op")
}

func benchCfg(b *testing.B) msdrv.Config {
	b.Helper()
	a := os.Getenv("STORM_MSSQL_ADDR")
	if a == "" {
		b.Skip("STORM_MSSQL_ADDR unset")
	}
	return msdrv.Config{
		Addr: a, User: "sa", Password: os.Getenv("STORM_MSSQL_PASSWORD"),
		Database: "storm", TLS: msdrv.TLSDisabled,
	}
}
