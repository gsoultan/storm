// Separate module: a spike, and nothing in storm should link it.
module github.com/gsoultan/storm/v2/internal/mysqlspike/wire

go 1.27.3

require github.com/gsoultan/storm/v2 v2.0.0

replace github.com/gsoultan/storm/v2 => ../../..
