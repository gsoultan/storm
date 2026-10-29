// Package pgstat reads what a PostgreSQL server knows about its sessions:
// which one a connection is, what each is doing, which locks it holds or waits
// for, and who it waits behind.
//
// For tests and diagnostics about concurrency, which have to see one session
// waiting for another to know they made the interleaving they are about. The
// catalogs it reads are PostgreSQL's own; nothing here is generated from a
// model, and no other back end has them.
package pgstat
