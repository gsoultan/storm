module example.com/orders

go 1.27

replace github.com/gsoultan/storm => ../..

require (
	github.com/go-kit/kit v0.13.0
	github.com/go-kit/log v0.2.1
	github.com/gsoultan/storm v0.0.0-00010101000000-000000000000
	github.com/jackc/pgx/v5 v5.10.0
)

require (
	github.com/go-logfmt/logfmt v0.5.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)
