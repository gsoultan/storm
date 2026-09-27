package model

import "github.com/gsoultan/storm"

type CountRow struct{ N int64 }

// OpenCount is a sharded query, discovered in the call form.
var OpenCount = storm.ShardedSQL[CountRow](`SELECT count(*) FROM orders WHERE tenant_id = $1`)

// CloseAll is a sharded statement, discovered in the call form.
var CloseAll = storm.ShardedSQLExec(`UPDATE orders SET status = 'closed' WHERE tenant_id = $1`)

// Typed is a sharded query, discovered from its declared type.
var Typed *storm.ShardedSQLQuery[CountRow] = storm.ShardedSQL[CountRow](`SELECT 1`)

// TypedExec is a sharded statement, discovered from its declared type.
var TypedExec *storm.ShardedSQLStmt = storm.ShardedSQLExec(`SELECT 2`)

// Plain is an ordinary declaration beside them, found the ordinary way.
var Plain = storm.SQL[CountRow](`SELECT 3`)

// unexported cannot be reached from a generated bootstrap.
var unexported = storm.ShardedSQLExec(`DELETE FROM nothing`)
