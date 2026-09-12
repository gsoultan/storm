// Separate module: a spike, and nothing in storm should link it.
module github.com/gsoultan/storm/internal/mysqlspike/wire

go 1.27

require github.com/gsoultan/storm v0.0.0

replace github.com/gsoultan/storm => ../../..
