// A SEPARATE module on purpose: storm's own go.mod gains no Oracle dependency
// for a measurement, exactly as internal/mssqlspike gains it no SQL Server one.
module github.com/gsoultan/storm/internal/oraclespike

go 1.27

replace github.com/gsoultan/storm => ../..

require (
	github.com/gsoultan/storm v0.0.0
	github.com/sijms/go-ora/v2 v2.9.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.11.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)
