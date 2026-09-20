// A SEPARATE module on purpose: storm's own go.mod gains no SQL Server
// dependency for a measurement, exactly as internal/mysqlspike gains it no
// MySQL one.
module github.com/gsoultan/storm/internal/mssqlspike

go 1.27

require (
	github.com/google/uuid v1.6.0
	github.com/gsoultan/storm v0.0.0
	github.com/microsoft/go-mssqldb v1.11.0
)

require (
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/shopspring/decimal v1.4.0 // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)

replace github.com/gsoultan/storm => ../..
