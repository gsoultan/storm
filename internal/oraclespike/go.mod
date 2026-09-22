// A SEPARATE module on purpose: storm's own go.mod gains no Oracle dependency
// for a measurement, exactly as internal/mssqlspike gains it no SQL Server one.
module github.com/gsoultan/storm/internal/oraclespike

go 1.27

replace github.com/gsoultan/storm => ../..

require (
	github.com/gsoultan/storm v0.0.0
	github.com/sijms/go-ora/v2 v2.9.0
)
