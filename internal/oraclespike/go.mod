// A SEPARATE module on purpose: storm's own go.mod gains no Oracle dependency
// for a measurement, exactly as internal/mssqlspike gains it no SQL Server one.
module github.com/gsoultan/storm/v2/internal/oraclespike

go 1.27.4

replace github.com/gsoultan/storm/v2 => ../..

require (
	github.com/gsoultan/storm/v2 v2.0.0
	github.com/sijms/go-ora/v2 v2.9.0
)

require github.com/gsoultan/caduceus v0.2.0 // indirect
