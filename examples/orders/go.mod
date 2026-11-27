module example.com/orders

go 1.27.3

replace github.com/gsoultan/storm => ../..

require (
	github.com/go-kit/kit v0.13.0
	github.com/go-kit/log v0.2.1
	github.com/gsoultan/caduceus v0.1.1-0.20261127082938-346a3bade389
	github.com/gsoultan/storm v0.0.0-00010101000000-000000000000
)

require github.com/go-logfmt/logfmt v0.5.1 // indirect
