module example.com/orders

go 1.27.4

replace github.com/gsoultan/storm/v2 => ../..

require (
	github.com/go-kit/kit v0.13.0
	github.com/go-kit/log v0.2.1
	github.com/gsoultan/caduceus v0.1.1-0.20261127084643-d08298f9aa90
	github.com/gsoultan/storm/v2 v2.0.0
)

require github.com/go-logfmt/logfmt v0.5.1 // indirect
