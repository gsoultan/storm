// A SEPARATE module on purpose, as internal/oraclespike is: storm's own
// go.mod gains no SQLite driver. storm reaches SQLite through database/sql and
// the program brings the driver; this module is that program.
module github.com/gsoultan/storm/v2/internal/sqlitecheck

go 1.27.4

replace github.com/gsoultan/storm/v2 => ../..

require (
	github.com/gsoultan/storm/v2 v2.0.0
	modernc.org/sqlite v1.63.0
)

require (
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gsoultan/caduceus v0.2.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.49.0 // indirect
	modernc.org/libc v1.80.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
